package object

import (
	"sync"
	"time"

	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/log"
	"github.com/named-data/ndnd/std/ndn"
	mgmt "github.com/named-data/ndnd/std/ndn/mgmt_2022"
	sig "github.com/named-data/ndnd/std/security/signer"
	"github.com/named-data/ndnd/std/types/optional"
	"github.com/named-data/ndnd/std/utils"
)

var announceMutex sync.Mutex

// (AI GENERATED DESCRIPTION): AnnouncePrefix registers the supplied prefix announcement in the client’s internal map and, if the NDN face is running, launches a goroutine to transmit the announcement to peers.
func (c *Client) AnnouncePrefix(args ndn.Announcement) {
	hash := args.Name.TlvStr()
	c.announcements.Store(hash, args)

	if c.engine.Face().IsRunning() {
		go c.announcePrefix_(args)
	}
}

// (AI GENERATED DESCRIPTION): Deletes the client’s stored announcement for the specified prefix name and, if the network engine’s face is running, asynchronously initiates its withdrawal.
func (c *Client) WithdrawPrefix(name enc.Name, onError func(error)) {
	hash := name.TlvStr()
	ann, ok := c.announcements.LoadAndDelete(hash)
	if !ok {
		return
	}

	if c.engine.Face().IsRunning() {
		go c.withdrawPrefix_(ann.(ndn.Announcement), onError)
	}
}

// (AI GENERATED DESCRIPTION): Announces a prefix to the network by registering it with the PET (add‑nexthop), optionally setting its cost, and spacing the request with a short delay to accommodate NFD behavior.
func (c *Client) announcePrefix_(args ndn.Announcement) {
	announceMutex.Lock()
	time.Sleep(1 * time.Millisecond) // thanks NFD
	announceMutex.Unlock()

	hasLocalForwarder := c.engine.Face().IsLocal()
	useLocalRouting := hasLocalForwarder && args.Expose && c.preferLocalRouting
	if useLocalRouting {
		ctrlArgs := &mgmt.ControlArgs{
			Name: args.Name,
			Cost: optional.Some(args.Cost),
		}
		if args.Multicast {
			ctrlArgs.Multicast = true
		}
		_, err := mgmt.ExecServiceCmd(
			c.engine, true, "dv", "prefix", "announce",
			ctrlArgs,
			&ndn.InterestConfig{
				Lifetime:    optional.Some(commandTimeout),
				Nonce:       utils.ConvertNonce(c.engine.Timer().Nonce()),
				MustBeFresh: true,
				SigNonce:    c.engine.Timer().Nonce(),
				SigTime:     optional.Some(time.Duration(c.engine.Timer().Now().UnixMilli()) * time.Millisecond),
			},
			sig.NewSha256Signer(),
			nil,
		)
		if err != nil {
			log.Warn(c, "Failed to announce prefix through local routing daemon", "name", args.Name, "err", err)
			if args.OnError != nil {
				args.OnError(err)
			}
		} else {
			log.Info(c, "Exposed prefix through local routing daemon", "name", args.Name)
		}
		return
	}

	if hasLocalForwarder {
		if _, err := c.engine.ExecMgmtCmd("pet", "add-nexthop", &mgmt.ControlArgs{
			Name: args.Name,
			Cost: optional.Some(args.Cost),
		}); err != nil {
			log.Warn(c, "Failed to register prefix in local PET", "name", args.Name, "err", err)
			if args.OnError != nil {
				args.OnError(err)
			}
			return
		}
		log.Info(c, "Registered prefix in local PET", "name", args.Name)
	}

	if !args.Expose {
		return
	}

	if err := c.insertPrefix(args, false); err != nil {
		log.Warn(c, "Failed to expose prefix to router through prefix insertion", "name", args.Name, "err", err)
		if args.OnError != nil {
			args.OnError(err)
		}
	} else {
		log.Info(c, "Exposed prefix to router through prefix insertion", "name", args.Name)
	}
}

// (AI GENERATED DESCRIPTION): Withdraws a previously announced prefix from the local PET by issuing a “pet remove‑nexthop” command and logs the result.
func (c *Client) withdrawPrefix_(args ndn.Announcement, onError func(error)) {
	announceMutex.Lock()
	time.Sleep(1 * time.Millisecond) // thanks NFD
	announceMutex.Unlock()

	hasLocalForwarder := c.engine.Face().IsLocal()
	useLocalRouting := hasLocalForwarder && args.Expose && c.preferLocalRouting
	if useLocalRouting {
		_, err := mgmt.ExecServiceCmd(
			c.engine, true, "dv", "prefix", "withdraw",
			&mgmt.ControlArgs{
				Name: args.Name,
			},
			&ndn.InterestConfig{
				Lifetime:    optional.Some(commandTimeout),
				Nonce:       utils.ConvertNonce(c.engine.Timer().Nonce()),
				MustBeFresh: true,
				SigNonce:    c.engine.Timer().Nonce(),
				SigTime:     optional.Some(time.Duration(c.engine.Timer().Now().UnixMilli()) * time.Millisecond),
			},
			sig.NewSha256Signer(),
			nil,
		)
		if err != nil {
			log.Warn(c, "Failed to withdraw prefix through local routing daemon", "name", args.Name, "err", err)
			if onError != nil {
				onError(err)
			}
		} else {
			log.Info(c, "Withdrew prefix through local routing daemon", "name", args.Name)
		}
		return
	}

	if hasLocalForwarder {
		if _, err := c.engine.ExecMgmtCmd("pet", "remove-nexthop", &mgmt.ControlArgs{
			Name: args.Name,
		}); err != nil {
			log.Warn(c, "Failed to unregister prefix from local PET", "name", args.Name, "err", err)
			if onError != nil {
				onError(err)
			}
			return
		}
		log.Info(c, "Unregistered prefix from local PET", "name", args.Name)
	}

	if !args.Expose {
		return
	}

	if err := c.insertPrefix(args, true); err != nil {
		log.Warn(c, "Failed to withdraw prefix through router insertion", "name", args.Name, "err", err)
		if onError != nil {
			onError(err)
		}
	} else {
		log.Info(c, "Withdrew prefix through router insertion", "name", args.Name)
	}
}

// (AI GENERATED DESCRIPTION): Re‑issues all stored announcements asynchronously when the Face comes up, stopping the iteration if the Face ever stops running.
func (c *Client) onFaceUp() {
	go func() {
		if err := c.setupDefaultRoute(); err != nil {
			log.Warn(c, "Failed to configure default router route on face-up", "err", err)
		}
		c.announcements.Range(func(key, value any) bool {
			c.announcePrefix_(value.(ndn.Announcement))
			return c.engine.Face().IsRunning()
		})
	}()
}
