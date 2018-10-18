// Package starlight exposes a payment channel agent on the Stellar network.
package starlight

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	bolt "github.com/coreos/bbolt"
	b "github.com/stellar/go/build"
	"github.com/stellar/go/clients/horizon"
	"github.com/stellar/go/network"
	"github.com/stellar/go/xdr"
	"golang.org/x/crypto/bcrypt"

	"github.com/interstellar/starlight/errors"
	"github.com/interstellar/starlight/net"
	"github.com/interstellar/starlight/starlight/db"
	"github.com/interstellar/starlight/starlight/fsm"
	"github.com/interstellar/starlight/starlight/internal/update"
	"github.com/interstellar/starlight/starlight/key"
	"github.com/interstellar/starlight/starlight/taskbasket"
	"github.com/interstellar/starlight/starlight/xlm"
	"github.com/interstellar/starlight/worizon"
)

var (
	errAcctsSame           = errors.New("same host and guest acct address")
	errAlreadyConfigured   = errors.New("already configured")
	ErrExists              = errors.New("channel exists")
	errInsufficientBalance = errors.New("insufficient balance")
	errInvalidEdit         = errors.New("can only update password and horizon URL")
	errInvalidHorizonURL   = errors.New("invalid Horizon URL")
	errInvalidPassword     = errors.New("invalid password")
	errInvalidUsername     = errors.New("invalid username")
	errNotConfigured       = errors.New("not configured")
	errNotFunded           = errors.New("primary acct not funded")
	errPasswordsDontMatch  = errors.New("old password doesn't match")
	errAccountNotFound     = errors.New("account not found")
	errEmptyAddress        = errors.New("destination address not set")
	errEmptyAmount         = errors.New("amount not set")
	errNoChannelSpecified  = errors.New("channel not specified")
)

// An Agent acts on behalf of the user to open, close,
// maintain, and use payment channels.
// Its methods are safe to call concurrently.
// Methods 'Do*' initiate channel operations.
//
// An Agent serializes all its state changes
// and stores them in a database as Update records.
// Methods Wait and Updates provide synchronization
// and access (respectively) for updates.
type Agent struct {
	// An Agent has three levels of readiness:
	//
	//   1. brand new, not ready at all
	//   2. configured, but account not created yet
	//   3. fully ready, account created & funded
	//
	// The conventional way to distinguish these is by checking
	// the database for the presence of the Horizon URL and the
	// primary account's sequence number. Helper functions
	// isReadyConfigured and isReadyFunded do these checks.

	once    sync.Once // build handler
	handler http.Handler

	evcond sync.Cond

	// This is the context object passed to Agent.start (whether via StartAgent or ConfigInit).
	// It is used to create child contexts when starting new channels.
	ctx context.Context

	// Secret-key entropy seed; can be nil, see Authenticate.
	// Used to generate account keypairs.
	//
	// When seed is nil, there are many FSM inputs we can't handle.
	// We attempt to handle all inputs regardless, and if there's a
	// problem, such as seed being nil, we roll back the database
	// transaction. If the update was the result of a peer message,
	// we return an error to the peer, which will resend its message
	// later. Eventually, the local user will supply the password to
	// decrypt the seed, and then we'll be able to handle resent
	// messages (as well as all new inputs).
	seed []byte // write-once; synchronized with db.Update

	// Horizon client wrapper.
	wclient worizon.Client

	// HTTP client used for agent requests. Treated as immutable state
	// after agent creation.
	httpclient http.Client

	tb *taskbasket.TB

	wg *sync.WaitGroup

	db *bolt.DB // doubles as a mutex for the fields in this struct

	// Maps Starlight channel IDs to cancellation functions.
	// Call the cancellation function to stop the goroutines associated with the channel.
	cancelers map[string]context.CancelFunc
}

// Config has user-facing, primary options for the Starlight agent
type Config struct {
	Username string `json:",omitempty"`
	Password string `json:",omitempty"`
	// WARNING: this software is not compatible with Stellar mainnet.
	HorizonURL string `json:",omitempty"`

	// OldPassword is required from the client in ConfigEdit
	// when changing the password.
	// It's never included in Updates.
	OldPassword string `json:",omitempty"`

	MaxRoundDurMin   int64      `json:",omitempty"`
	FinalityDelayMin int64      `json:",omitempty"`
	ChannelFeerate   xlm.Amount `json:",omitempty"`
	HostFeerate      xlm.Amount `json:",omitempty"`

	// KeepAlive, if set, indicates whether or not the agent will
	// send 0-value keep-alive payments on its channels
	KeepAlive *bool `json:",omitempty"`
}

const tbBucket = "tasks"

// StartAgent starts an agent
// using the bucket "agent" in db for storage
// and returns it.
func StartAgent(ctx context.Context, boltDB *bolt.DB) (*Agent, error) {
	g := &Agent{
		db:        boltDB,
		cancelers: make(map[string]context.CancelFunc),
		wg:        new(sync.WaitGroup),
	}

	g.evcond.L = new(sync.Mutex)

	err := db.Update(boltDB, func(root *db.Root) error { return g.start(ctx, root) })
	if err != nil {
		return nil, err
	}

	g.tb, err = taskbasket.New(ctx, boltDB, []byte(tbBucket), tbCodec{g: g})
	if err != nil {
		return nil, err
	}

	g.allez(func() { g.tb.Run(ctx) })

	return g, nil
}

// Must be called from within an update transaction.
func (g *Agent) start(ctx context.Context, root *db.Root) error {
	if !g.isReadyConfigured(root) {
		return nil
	}

	g.ctx = ctx
	// WARNING: this software is not compatible with Stellar mainnet.
	g.wclient.SetURL(root.Agent().Config().HorizonURL())

	chans := root.Agent().Channels()

	var chanIDs []string
	err := chans.Bucket().ForEach(func(chanID, _ []byte) error {
		chanIDs = append(chanIDs, string(chanID))
		return nil
	})
	if err != nil {
		return err
	}

	for _, chanID := range chanIDs {
		err := g.startChannel(ctx, root, chanID)
		if err != nil {
			return err
		}
	}

	primaryAcct := root.Agent().PrimaryAcct().Address()
	w := root.Agent().Wallet()

	g.allez(func() { g.watchWalletAcct(ctx, primaryAcct, horizon.Cursor(w.Cursor)) })

	return nil
}

// allez launches f as a goroutine, tracking it in the agent's WaitGroup.
func (g *Agent) allez(f func()) {
	g.wg.Add(1)
	go func() {
		f()
		g.wg.Done()
	}()
}

// Wait waits for the agent's goroutines to exit (by waiting on the agent's WaitGroup).
func (g *Agent) Wait() {
	g.wg.Wait()
}

// ConfigInit sets g's configuration,
// generates a private key for the wallet,
// and performs any other necessary setup steps,
// such as obtaining free testnet lumens.
// It is an error if g has already been configured.
func (g *Agent) ConfigInit(ctx context.Context, c *Config) error {
	err := g.wclient.ValidateTestnetURL(c.HorizonURL)
	if err != nil {
		return err
	}

	return db.Update(g.db, func(root *db.Root) error {
		if g.isReadyConfigured(root) {
			return errAlreadyConfigured
		}

		g.seed = make([]byte, 32)
		randRead(g.seed)
		k := key.DeriveAccountPrimary(g.seed)
		primaryAcct := fsm.AccountId(key.PublicKeyXDR(k))

		if len(c.Password) > 72 {
			return errors.Wrap(errInvalidPassword, "too long (max 72 chars)") // bcrypt limit
		}
		if c.Password == "" {
			return errors.Wrap(errInvalidPassword, "empty password")
		}
		if !validateUsername(c.Username) {
			return errInvalidUsername
		}
		digest, err := bcrypt.GenerateFromPassword([]byte(c.Password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		root.Agent().Config().PutUsername(c.Username)
		root.Agent().Config().PutPwType("bcrypt")
		root.Agent().Config().PutPwHash(digest[:])
		root.Agent().Config().PutHorizonURL(c.HorizonURL)
		root.Agent().PutEncryptedSeed(sealBox(g.seed, []byte(c.Password)))
		root.Agent().PutNextKeypathIndex(1)
		root.Agent().PutPrimaryAcct(&primaryAcct)
		if c.MaxRoundDurMin == 0 {
			c.MaxRoundDurMin = defaultMaxRoundDurMin
		}
		if c.FinalityDelayMin == 0 {
			c.FinalityDelayMin = defaultFinalityDelayMin
		}
		if c.ChannelFeerate == 0 {
			c.ChannelFeerate = defaultChannelFeerate
		}
		if c.HostFeerate == 0 {
			c.HostFeerate = defaultHostFeerate
		}
		if c.KeepAlive == nil {
			c.KeepAlive = new(bool)
			*c.KeepAlive = true
		}
		root.Agent().Config().PutMaxRoundDurMin(c.MaxRoundDurMin)
		root.Agent().Config().PutFinalityDelayMin(c.FinalityDelayMin)
		root.Agent().Config().PutChannelFeerate(int64(c.ChannelFeerate))
		root.Agent().Config().PutHostFeerate(int64(c.HostFeerate))
		root.Agent().Config().PutKeepAlive(*c.KeepAlive)

		w := &fsm.WalletAcct{
			Balance: xlm.Amount(0),
			Seqnum:  0,
			Cursor:  "",
		}
		root.Agent().PutWallet(w)
		// WARNING: this software is not compatible with Stellar mainnet.
		g.wclient.SetURL(c.HorizonURL)
		g.putUpdate(root, &Update{
			Type: update.InitType,
			Config: &update.Config{
				Username:   c.Username,
				Password:   "[redacted]",
				HorizonURL: c.HorizonURL,
			},
			Account: &update.Account{
				ID:      primaryAcct.Address(),
				Balance: 0,
			},
		})

		g.allez(func() { g.getTestnetFaucetFunds(primaryAcct) })

		return g.start(ctx, root)
	})
}

// ConfigEdit edits g's configuration.
// Only Password and HorizonURL can be changed;
// attempting to change another field is an error.
func (g *Agent) ConfigEdit(c *Config) error {
	// Can update password and/or horizon url;
	// attempting to update other fields is an error.
	if c.Username != "" {
		return errInvalidEdit
	}
	if len(c.Password) > 72 {
		return errors.Wrap(errInvalidPassword, "too long (max 72 chars)") // bcrypt limit
	}
	if c.Password == "" && c.HorizonURL == "" {
		return nil // nothing to do
	}
	if c.HorizonURL != "" {
		err := g.wclient.ValidateTestnetURL(c.HorizonURL)
		if err != nil {
			return err
		}
	}

	return db.Update(g.db, func(root *db.Root) error {
		if !g.isReadyConfigured(root) {
			return errNotConfigured
		}

		if c.Password != "" {
			if root.Agent().Config().PwType() != "bcrypt" {
				return nil
			}
			digest := root.Agent().Config().PwHash()
			err := bcrypt.CompareHashAndPassword(digest, []byte(c.OldPassword))
			if err != nil {
				return errPasswordsDontMatch
			}

			digest, err = bcrypt.GenerateFromPassword([]byte(c.Password), bcrypt.DefaultCost)
			if err != nil {
				return err
			}
			root.Agent().Config().PutPwType("bcrypt")
			root.Agent().Config().PutPwHash(digest[:])
			root.Agent().PutEncryptedSeed(sealBox(g.seed, []byte(c.Password)))
			g.putUpdate(root, &Update{
				Type:   update.ConfigType,
				Config: &update.Config{Password: "[redacted]"},
			})
		}

		// WARNING: this software is not compatible with Stellar mainnet.
		if c.HorizonURL != "" {
			root.Agent().Config().PutHorizonURL(c.HorizonURL)
			g.putUpdate(root, &Update{
				Type:   update.ConfigType,
				Config: &update.Config{HorizonURL: c.HorizonURL},
			})
			g.wclient.SetURL(c.HorizonURL)
		}
		return nil
	})
}

// Configured returns whether ConfigInit has been called on g.
func (g *Agent) Configured() bool {
	var ok bool
	db.View(g.db, func(root *db.Root) error {
		ok = g.isReadyConfigured(root)
		return nil
	})
	return ok
}

func (g *Agent) isReadyConfigured(root *db.Root) bool {
	return root.Agent().Config().HorizonURL() != ""
}

func (g *Agent) isReadyFunded(root *db.Root) bool {
	return root.Agent().Wallet().Seqnum > 0
}

// Function watchWalletAcct runs in its own goroutine waiting for creation of the wallet account,
// and payments or merges into it.
// When such transactions hit the ledger,
// it reports an *Update back for the client to consume.
func (g *Agent) watchWalletAcct(ctx context.Context, acctID string, cursor horizon.Cursor) {
	err := g.wclient.StreamTxs(ctx, acctID, cursor, func(htx worizon.Tx) error {
		InputTx, err := fsm.NewTx(&htx)
		if err != nil {
			return err
		}
		if InputTx.Result.Result.Code != xdr.TransactionResultCodeTxSuccess {
			// Ignore failed txs.
			return nil
		}
		db.Update(g.db, func(root *db.Root) error {
			// log succcessfully sent transactions
			if InputTx.Env.Tx.SourceAccount.Address() == acctID {
				w := root.Agent().Wallet()
				w.Cursor = htx.PT
				root.Agent().PutWallet(w)
				g.putUpdate(root, &Update{
					Type:    update.TxSuccessType,
					InputTx: InputTx,
				})
			}
			for index, op := range InputTx.Env.Tx.Operations {
				switch op.Body.Type {
				case xdr.OperationTypeCreateAccount:
					createAccount := op.Body.CreateAccountOp
					if createAccount.Destination.Address() != acctID {
						continue
					}

					// compute the initial sequence number of the account
					// it's the ledger number of the transaction that created it, shifted left 32 bits
					seqnum := xdr.SequenceNumber(uint64(htx.Ledger) << 32)

					w := &fsm.WalletAcct{
						Balance: xlm.Amount(createAccount.StartingBalance),
						Seqnum:  seqnum,
						Cursor:  htx.PT,
					}
					root.Agent().PutWallet(w)
					g.putUpdate(root, &Update{
						Type: update.AccountType,
						Account: &update.Account{
							ID:      acctID,
							Balance: uint64(w.Balance),
						},
						InputTx: InputTx,
						OpIndex: index,
					})

				case xdr.OperationTypePayment:
					payment := op.Body.PaymentOp
					if payment.Destination.Address() != acctID {
						continue
					}
					// Ignore payments that are not in lumens
					if payment.Asset.Type != xdr.AssetTypeAssetTypeNative {
						continue
					}
					w := root.Agent().Wallet()
					w.Balance += xlm.Amount(payment.Amount)
					w.Cursor = htx.PT
					root.Agent().PutWallet(w)
					g.putUpdate(root, &Update{
						Type: update.AccountType,
						Account: &update.Account{
							ID:      acctID,
							Balance: uint64(w.Balance),
						},
						InputTx: InputTx,
						OpIndex: index,
					})

				case xdr.OperationTypeAccountMerge:
					if op.Body.Destination.Address() != acctID {
						continue
					}

					// Note: account merge amounts are always in lumens.
					// See https://www.stellar.org/developers/guides/concepts/list-of-operations.html#account-merge.

					// If the tx is successful and InputTx.Env.Tx.Operations[index] is an account merge,
					// we can depend on (*InputTx.Result.Result.Results)[index].Tr being present and having an AccountMergeResult.
					mergeAmount := *(*InputTx.Result.Result.Results)[index].Tr.AccountMergeResult.SourceAccountBalance

					w := root.Agent().Wallet()
					w.Balance += xlm.Amount(mergeAmount)
					w.Cursor = htx.PT
					root.Agent().PutWallet(w)

					g.putUpdate(root, &Update{
						Type: update.AccountType,
						Account: &update.Account{
							ID:      acctID,
							Balance: uint64(w.Balance),
						},
						InputTx: InputTx,
						OpIndex: index,
					})
				}
			}
			return nil
		})
		return nil
	})
	if err != nil {
		log.Fatalf("watching wallet-account txs: %s", err)
	}
}

func (g *Agent) getTestnetFaucetFunds(acctID fsm.AccountId) {
	// The faucet is not 100% reliable (it often times out),
	// so this tries five times before giving up.
	// On failure, it reports the result as an *Update for the
	// client to consume.
	backoff := &net.Backoff{Base: 100 * time.Millisecond}

	for i := 0; i < 5; i++ {
		resp, err := g.httpclient.Get("https://friendbot.stellar.org/?addr=" + acctID.Address())
		if err != nil {
			dur := backoff.Next()
			log.Printf("retrieving testnet funds for %s: %s (will retry in %s)", acctID.Address(), err, dur)
			time.Sleep(dur)
			continue
		}
		if resp.StatusCode/100 != 2 {
			var v struct {
				Detail      string
				ResultCodes json.RawMessage `json:"result_codes"`
			}
			err := json.NewDecoder(resp.Body).Decode(&v)
			var warning string
			if err != nil {
				warning = "bad http status from faucet: " + resp.Status
				warning += "cannot read faucet response: " + err.Error()
			} else {
				warning = fmt.Sprintf("faucet: %s (%s)", v.Detail, v.ResultCodes)
			}
			db.Update(g.db, func(root *db.Root) error {
				g.putUpdate(root, &Update{
					Type:    update.WarningType,
					Warning: warning,
				})
				return nil
			})
			dur := backoff.Next()
			log.Printf("Retrieving testnet funds for %s (will retry in %s)", acctID.Address(), dur)
			time.Sleep(dur)
			continue
		}
		return
	}
	db.Update(g.db, func(root *db.Root) error {
		g.putUpdate(root, &Update{
			Type:    update.WarningType,
			Warning: "could not retrieve testnet faucet funds",
		})
		return nil
	})
}

// Authenticate authenticates the given user name and password.
// If they're valid, it also decrypts the secret entropy seed
// if necessary, allowing private-key operations to proceed.
//
// It returns whether name and password are valid.
func (g *Agent) Authenticate(name, password string) bool {
	var ok bool
	var seed []byte
	if !validateUsername(name) {
		return false
	}
	db.View(g.db, func(root *db.Root) error {
		if !g.isReadyConfigured(root) {
			return nil
		}
		if name != root.Agent().Config().Username() {
			return nil
		}
		if root.Agent().Config().PwType() != "bcrypt" {
			return nil
		}
		digest := root.Agent().Config().PwHash()
		err := bcrypt.CompareHashAndPassword(digest, []byte(password))
		ok = err == nil
		seed = g.seed
		return nil
	})
	if ok && seed == nil {
		err := db.Update(g.db, func(root *db.Root) error {
			if g.seed != nil {
				return nil // already decrypted
			}
			encseed := root.Agent().EncryptedSeed()
			g.seed = openBox(encseed, []byte(password))
			return nil
		})
		if err != nil {
			panic(err)
		}
	}
	return ok
}

const (
	defaultMaxRoundDurMin   = 24 * 60
	defaultFinalityDelayMin = 4 * 60
	defaultChannelFeerate   = 10 * xlm.Millilumen
	defaultHostFeerate      = 100 * xlm.Stroop
)

func (g *Agent) checkChannelUnique(a, b string) error {
	return db.View(g.db, func(root *db.Root) error {
		chans := root.Agent().Channels()
		return chans.Bucket().ForEach(func(currChanID, _ []byte) error {
			c := chans.Get(currChanID)
			p, q := c.HostAcct.Address(), c.GuestAcct.Address()
			if (a == p && b == q) || (a == q && b == p) {
				return errors.Wrapf(ErrExists, "between host %s and guest %s", p, q)
			}
			return nil
		})
	})
}

// DoCreateChannel creates a channel between the agent host and the guest
// specified at guestFedAddr, funding the channel with hostAmount
func (g *Agent) DoCreateChannel(guestFedAddr string, hostAmount xlm.Amount, hostURL string) (*fsm.Channel, error) {
	if guestFedAddr == "" {
		return nil, errEmptyAddress
	}
	if hostAmount == 0 {
		return nil, errEmptyAmount
	}
	// TODO(debnil): Distinguish account string and federation server address better, i.e. using type aliases for string.
	var hostAcctStr string
	db.View(g.db, func(root *db.Root) error {
		hostAcctStr = root.Agent().PrimaryAcct().Address()
		return nil
	})

	guestAcctStr, starlightURL, err := g.FindAccount(guestFedAddr)
	if err != nil {
		return nil, errors.Wrapf(err, "finding account %s", guestFedAddr)
	}
	if guestAcctStr == hostAcctStr {
		return nil, errAcctsSame
	}
	err = g.checkChannelUnique(hostAcctStr, guestAcctStr)
	if err != nil {
		return nil, err
	}

	var ch *fsm.Channel
	err = db.Update(g.db, func(root *db.Root) error {
		if !g.isReadyFunded(root) {
			return errNotFunded
		}

		w := root.Agent().Wallet()
		w.Seqnum += 3
		w.Address = root.Agent().Config().Username() + "*" + hostURL

		// Local node is the host.
		// Remote node is the guest.

		var guestAcct fsm.AccountId
		err := guestAcct.SetAddress(guestAcctStr)
		if err != nil {
			return errors.Wrapf(err, "setting guest address %s", guestAcctStr)
		}

		channelKeyIndex := nextChannelKeyIndex(root.Agent(), 3)
		channelKeyPair := key.DeriveAccount(g.seed, channelKeyIndex)
		channelID := channelKeyPair.Address()

		var escrowAcct fsm.AccountId
		err = escrowAcct.SetAddress(channelKeyPair.Address())
		if err != nil {
			return errors.Wrapf(err, "setting escrow address %s", channelKeyPair.Address())
		}

		firstThrowawayKeyPair := key.DeriveAccount(g.seed, channelKeyIndex+1)
		var hostRatchetAcct fsm.AccountId
		err = hostRatchetAcct.SetAddress(firstThrowawayKeyPair.Address())
		if err != nil {
			return errors.Wrapf(err, "setting host ratchet address %s", firstThrowawayKeyPair.Address())
		}

		secondThrowawayKeyPair := key.DeriveAccount(g.seed, channelKeyIndex+2)
		var guestRatchetAcct fsm.AccountId
		err = guestRatchetAcct.SetAddress(secondThrowawayKeyPair.Address())
		if err != nil {
			return errors.Wrapf(err, "setting guest ratchet address %s", secondThrowawayKeyPair.Address())
		}

		fundingTime := g.wclient.Now()

		if ch = g.getChannel(root, channelID); ch.State != fsm.Start {
			return errors.Wrap(ErrExists, string(channelID))
		}

		ch = &fsm.Channel{
			ID:                  channelID,
			Role:                fsm.Host,
			HostAmount:          hostAmount,
			CounterpartyAddress: guestFedAddr,
			RemoteURL:           starlightURL,
			Passphrase:          g.passphrase(root),
			MaxRoundDuration:    time.Duration(root.Agent().Config().MaxRoundDurMin()) * time.Minute,
			FinalityDelay:       time.Duration(root.Agent().Config().FinalityDelayMin()) * time.Minute,
			ChannelFeerate:      xlm.Amount(root.Agent().Config().ChannelFeerate()),
			HostFeerate:         xlm.Amount(root.Agent().Config().HostFeerate()),
			FundingTime:         fundingTime,
			PaymentTime:         fundingTime,
			KeyIndex:            channelKeyIndex,
			GuestAcct:           guestAcct,
			EscrowAcct:          escrowAcct,
			HostRatchetAcct:     hostRatchetAcct,
			GuestRatchetAcct:    guestRatchetAcct,
			RoundNumber:         1,
		}
		err = ch.HostAcct.SetAddress(hostAcctStr)
		if err != nil {
			return errors.Wrap(err, "setting host address")
		}
		newBalance := w.Balance - ch.SetupAndFundingReserveAmount()
		if newBalance < 0 {
			return errors.Wrap(errInsufficientBalance, w.Balance.String())
		}
		w.Balance = newBalance
		g.putChannel(root, channelID, ch)
		root.Agent().PutWallet(w)

		return g.doUpdateChannel(root, ch.ID, func(root *db.Root, updater *fsm.Updater, update *Update) error {
			c := &fsm.Command{
				UserCommand: fsm.CreateChannel,
				Amount:      ch.HostAmount,
				Recipient:   guestFedAddr,
			}
			update.InputCommand = c
			return updater.Cmd(c)
		})
	})
	return ch, err
}

func (g *Agent) DoWalletPay(dest string, amount xlm.Amount) error {
	if dest == "" {
		return errEmptyAddress
	}
	if amount == 0 {
		return errEmptyAmount
	}
	return db.Update(g.db, func(root *db.Root) error {
		w := root.Agent().Wallet()
		if w.Balance <= amount+xlm.Amount(root.Agent().Config().HostFeerate()) {
			return errors.New("insufficient funds")
		}

		w.Balance -= amount
		w.Balance -= xlm.Amount(root.Agent().Config().HostFeerate())
		w.Seqnum++
		root.Agent().PutWallet(w)
		hostAcct := root.Agent().PrimaryAcct()
		btx, err := b.Transaction(
			b.Network{Passphrase: g.passphrase(root)},
			b.SourceAccount{AddressOrSeed: hostAcct.Address()},
			b.Sequence{Sequence: uint64(w.Seqnum)},
			b.Payment(
				b.SourceAccount{AddressOrSeed: hostAcct.Address()},
				b.Destination{AddressOrSeed: dest},
				b.NativeAmount{Amount: amount.HorizonString()},
			),
		)
		if err != nil {
			return err
		}
		k := key.DeriveAccountPrimary(g.seed)
		env, err := btx.Sign(k.Seed())
		if err != nil {
			return err
		}
		time := g.wclient.Now()
		g.putUpdate(root, &Update{
			Type: update.AccountType,
			Account: &update.Account{
				ID:      hostAcct.Address(),
				Balance: uint64(w.Balance),
			},
			InputCommand: &fsm.Command{
				UserCommand: fsm.Pay,
				Amount:      amount,
				Recipient:   dest,
				Time:        time,
			},
			InputLedgerTime: time,
			PendingSequence: strconv.FormatInt(int64(w.Seqnum), 10),
		})
		return g.addTxTask(root.Tx(), walletBucket, *env.E)
	})
}

func (g *Agent) addTxTask(tx *bolt.Tx, chanID string, e xdr.TransactionEnvelope) error {
	t := &TbTx{
		g:      g,
		ChanID: chanID,
		E:      e,
	}
	return g.tb.AddTx(tx, t)
}

func (g *Agent) addMsgTask(tx *bolt.Tx, remoteURL string, msg *fsm.Message) error {
	m := &TbMsg{
		g:         g,
		RemoteURL: remoteURL,
		Msg:       *msg,
	}
	return g.tb.AddTx(tx, m)
}

// nextChannelKeyIndex reads the next unused key path index from bu
// and returns after bumping the stored key path index.
func nextChannelKeyIndex(agent *db.Agent, bump uint32) uint32 {
	i := agent.NextKeypathIndex()
	agent.PutNextKeypathIndex(i + bump)
	return i
}

// DoCommand executes c on channel channelID.
func (g *Agent) DoCommand(channelID string, c *fsm.Command) error {
	if len(channelID) == 0 {
		return errNoChannelSpecified
	}
	if c.UserCommand == "" {
		return errors.New("no command specified")
	}
	return g.updateChannel(channelID, func(_ *db.Root, updater *fsm.Updater, update *Update) error {
		update.InputCommand = c
		return updater.Cmd(c)
	})
}

func (g *Agent) scheduleTimer(tx *bolt.Tx, t time.Time, chanID string) {
	tx.OnCommit(func() {
		// TODO(bobg): this should be cancelable.
		g.wclient.AfterFunc(t, func() {
			err := g.updateChannel(chanID, func(_ *db.Root, updater *fsm.Updater, update *Update) error {
				update.InputLedgerTime = g.wclient.Now()
				return updater.Time()
			})
			if err != nil {
				log.Fatalf("scheduling timer on channel %s: %s", string(chanID), err)
			}
		})
	})
}

func (g *Agent) passphrase(root *db.Root) string {
	return network.TestNetworkPassphrase
}

// PeerHandler handles RPCs
// (such as ProposeChannel, AcceptChannel, Payment, etc.)
// from remote channel endpoints.
func (g *Agent) PeerHandler() http.Handler {
	g.once.Do(func() {
		mux := new(http.ServeMux)
		mux.HandleFunc("/starlight/message", g.handleMsg)
		mux.HandleFunc("/federation", g.handleFed)
		mux.HandleFunc("/.well-known/stellar.toml", g.handleTOML)
		g.handler = mux
	})
	return g.handler
}

func (g *Agent) handleMsg(w http.ResponseWriter, req *http.Request) {
	m := new(fsm.Message)
	err := json.NewDecoder(req.Body).Decode(m)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(m.ChannelID) == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var guestSeqNum, hostSeqNum, baseSeqNum xdr.SequenceNumber
	var starlightURL, hostAccount string
	if m.ChannelProposeMsg != nil {
		propose := m.ChannelProposeMsg
		err := g.checkChannelUnique(propose.HostAcct.Address(), propose.GuestAcct.Address())
		if err != nil {
			http.Error(w, "channel exists between parties", http.StatusResetContent)
			return
		}
		var escrowAcct xdr.AccountId
		err = escrowAcct.SetAddress(string(m.ChannelID))
		if err != nil {
			http.Error(w, "invalid channel ID", http.StatusBadRequest)
			return
		}
		baseSeqNum, guestSeqNum, hostSeqNum, err = g.getSequenceNumbers(m.ChannelID, propose.GuestRatchetAcct, propose.HostRatchetAcct)
		if err != nil {
			//TODO(debnil): StatusBadRequest implies a faulty input error. We may want to distinguish that
			//from other possible errors (e.g., network timeout).
			http.Error(w, "error fetching accounts", http.StatusBadRequest)
			return
		}
		hostAccount, starlightURL, err = g.FindAccount(m.ChannelProposeMsg.CounterpartyAddress)
		if starlightURL == "" {
			http.Error(w, "counterparty starlight URL not found", http.StatusBadRequest)
			return
		}
		if err != nil {
			errStr := fmt.Sprintf("counterparty starlight URL not found, got err %s", err)
			http.Error(w, errStr, http.StatusBadRequest)
			return
		}
		if hostAccount != m.ChannelProposeMsg.HostAcct.Address() {
			http.Error(w, fmt.Sprintf("host acct %s doesn't match acct %s retrieved from federation address %s",
				m.ChannelProposeMsg.HostAcct.Address(), hostAccount, m.ChannelProposeMsg.CounterpartyAddress), http.StatusBadRequest)
			return
		}
	}
	err = g.updateChannel(m.ChannelID, func(root *db.Root, updater *fsm.Updater, update *Update) error {
		if m.ChannelProposeMsg != nil {
			updater.C.GuestAcct = *root.Agent().PrimaryAcct()
			updater.C.GuestRatchetAcctSeqNum = guestSeqNum
			updater.C.HostRatchetAcctSeqNum = hostSeqNum
			updater.C.BaseSequenceNumber = baseSeqNum
			updater.C.RemoteURL = starlightURL
		}
		update.InputMessage = m
		return updater.Msg(m)
	})
	switch errors.Root(err) {
	case nil:
	case ErrExists, fsm.ErrChannelExists: // TODO(debnil): Add more non-retriable errors.
		// StatusResetContent is used to designate non-retriable errors.
		// TODO(debnil): Find a more suitable status code if possible.
		log.Printf("handling RPC message, channel %s: %s", string(m.ChannelID), err)
		http.Error(w, "non-retriable error", http.StatusResetContent)
		return
	default:
		log.Printf("handling RPC message, channel %s: %s", string(m.ChannelID), err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
}

func (g *Agent) handleFed(w http.ResponseWriter, req *http.Request) {
	if req.URL.Query().Get("type") != "name" {
		http.Error(w, "not implemented", http.StatusNotImplemented)
		return
	}

	var name, acct string
	db.View(g.db, func(root *db.Root) error {
		name = root.Agent().Config().Username()
		acct = root.Agent().PrimaryAcct().Address()
		return nil
	})

	q := req.URL.Query().Get("q")
	if q != name+"*"+req.Host {
		http.Error(w, "not found", 404)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{
		"stellar_address": q + "*" + req.Host,
		"account_id":      acct,
	})
}

func (g *Agent) handleTOML(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "text/plain")
	v := struct{ Origin string }{req.Host}
	tomlTemplate.Execute(w, v)
}

func (g *Agent) getSequenceNumbers(chanID string, guestRatchetAcct, hostRatchetAcct fsm.AccountId) (base, guest, host xdr.SequenceNumber, err error) {
	var escrowAcct xdr.AccountId
	err = escrowAcct.SetAddress(chanID)
	if err != nil {
		return 0, 0, 0, err
	}
	base, err = g.wclient.SequenceForAccount(escrowAcct.Address())
	if err != nil {
		return 0, 0, 0, err
	}
	guest, err = g.wclient.SequenceForAccount(guestRatchetAcct.Address())
	if err != nil {
		return 0, 0, 0, err
	}
	host, err = g.wclient.SequenceForAccount(hostRatchetAcct.Address())
	if err != nil {
		return 0, 0, 0, err
	}
	return base, guest, host, nil
}

var tomlTemplate = template.Must(template.New("toml").Parse(`
FEDERATION_SERVER="https://{{.Origin}}/federation"
STARLIGHT_SERVER="https://{{.Origin}}/"
`))
