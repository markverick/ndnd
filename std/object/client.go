package object

import (
	"fmt"
	"strings"
	"sync"

	enc "github.com/named-data/ndnd/std/encoding"
	eng "github.com/named-data/ndnd/std/engine"
	"github.com/named-data/ndnd/std/ndn"
	mgmt "github.com/named-data/ndnd/std/ndn/mgmt_2022"
	sec "github.com/named-data/ndnd/std/security"
	"github.com/named-data/ndnd/std/types/optional"
)

type Client struct {
	// underlying API engine
	engine ndn.Engine
	// data storage
	store ndn.Store
	// trust configuration
	trust *sec.TrustConfig
	// segment fetcher
	fetcher rrSegFetcher

	// announcements
	announcements sync.Map
	faceCancel    func()

	// Snapshot of client config used for expose/withdraw path selection.
	clientCfg          eng.ClientConfig
	preferLocalRouting bool
	routerSetupMu      sync.Mutex
	routerFaceID       optional.Optional[uint64]
}

// Create a new client with given engine and store
func NewClient(engine ndn.Engine, store ndn.Store, trust *sec.TrustConfig) ndn.Client {
	client := new(Client)
	client.engine = engine
	client.store = store
	client.trust = trust
	client.fetcher = newRrSegFetcher(client)
	client.clientCfg = eng.GetClientConfig()
	client.preferLocalRouting = client.clientCfg.RoutingMode == eng.ClientRoutingModeLocal

	client.announcements = sync.Map{}
	client.faceCancel = func() {}

	return client
}

// Instance log identifier
func (c *Client) String() string {
	return "client"
}

// Start the client. The engine must be running.
func (c *Client) Start() error {
	if !c.engine.IsRunning() {
		return fmt.Errorf("engine is not running")
	}

	if err := c.engine.AttachHandler(enc.Name{}, c.onInterest); err != nil {
		return err
	}
	if err := c.setupDefaultRoute(); err != nil {
		c.engine.DetachHandler(enc.Name{})
		return err
	}

	c.faceCancel = c.engine.Face().OnUp(c.onFaceUp)

	return nil
}

// Stop the client
func (c *Client) Stop() error {
	c.faceCancel()

	if err := c.engine.DetachHandler(enc.Name{}); err != nil {
		return err
	}

	return nil
}

// Get the underlying engine
func (c *Client) Engine() ndn.Engine {
	return c.engine
}

// Get the underlying store
func (c *Client) Store() ndn.Store {
	return c.store
}

// IsCongested returns true if the client is congested
func (c *Client) IsCongested() bool {
	return c.fetcher.IsCongested()
}

func (c *Client) setupDefaultRoute() error {
	if c.preferLocalRouting || !c.engine.Face().IsLocal() {
		return nil
	}

	routerUri := strings.TrimSpace(c.clientCfg.RouterUri)
	if routerUri == "" {
		return nil
	}

	c.routerSetupMu.Lock()
	defer c.routerSetupMu.Unlock()

	faceID, err := c.setupRouterFace(routerUri)
	if err != nil {
		return err
	}

	if current, ok := c.routerFaceID.Get(); ok && current == faceID {
		return nil
	}

	// Root prefix must be encoded as an explicit zero-length Name TLV.
	// NameFromStr("/") may produce a nil slice, which gets omitted in ControlArgs.
	rootPrefix := enc.Name{}
	_, err = c.engine.ExecMgmtCmd("pet", "add-nexthop", &mgmt.ControlArgs{
		Name:   rootPrefix,
		FaceId: optional.Some(faceID),
		Cost:   optional.Some(uint64(0)),
	})
	if err != nil {
		return fmt.Errorf("failed to install default router PET nexthop via face %d: %w", faceID, err)
	}

	c.routerFaceID = optional.Some(faceID)
	return nil
}

func (c *Client) setupRouterFace(routerUri string) (uint64, error) {
	raw, err := c.engine.ExecMgmtCmd("faces", "create", &mgmt.ControlArgs{
		Uri: optional.Some(routerUri),
	})
	if raw == nil {
		return 0, fmt.Errorf("failed to create router face %s: %w", routerUri, err)
	}

	resp, ok := raw.(*mgmt.ControlResponse)
	if !ok || resp == nil || resp.Val == nil || resp.Val.Params == nil || !resp.Val.Params.FaceId.IsSet() {
		return 0, fmt.Errorf("failed to resolve router face %s: unexpected response type %T", routerUri, raw)
	}

	// faces/create returns status 409 when a face with the same URI already exists.
	// Treat this as success and reuse the existing face ID from response params.
	if err != nil && resp.Val.StatusCode != 409 {
		return 0, fmt.Errorf("failed to resolve router face %s: %w", routerUri, err)
	}

	return resp.Val.Params.FaceId.Unwrap(), nil
}
