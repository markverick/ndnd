package sim

import (
	"fmt"

	"github.com/named-data/ndnd/fw/defn"
	"github.com/named-data/ndnd/fw/dispatch"
)

// FwSendFunc is the callback for outgoing packets from a forwarder face.
// faceID identifies the sending face; frame is the encoded LP packet bytes.
type FwSendFunc func(faceID uint64, frame []byte)

// DispatchFace implements dispatch.Face for the simulation environment.
// Each network interface (and the internal app face) on a simulated node
// is represented by one DispatchFace registered in dispatch.FaceDispatch.
type DispatchFace struct {
	faceID    uint64
	scope     defn.Scope
	linkType  defn.LinkType
	localURI  *defn.URI
	remoteURI *defn.URI
	state     defn.State
	onSend    FwSendFunc
}

var _ dispatch.Face = (*DispatchFace)(nil)

// NewDispatchFace creates a new simulation-mode dispatch face.
func NewDispatchFace(
	faceID uint64,
	scope defn.Scope,
	linkType defn.LinkType,
	onSend FwSendFunc,
) *DispatchFace {
	scheme := "sim-net"
	if scope == defn.Local {
		scheme = "sim-app"
	}
	return &DispatchFace{
		faceID:    faceID,
		scope:     scope,
		linkType:  linkType,
		localURI:  defn.DecodeURIString(fmt.Sprintf("%s://local/%d", scheme, faceID)),
		remoteURI: defn.DecodeURIString(fmt.Sprintf("%s://remote/%d", scheme, faceID)),
		state:     defn.Up,
		onSend:    onSend,
	}
}

func (f *DispatchFace) String() string {
	return fmt.Sprintf("sim-face-%d", f.faceID)
}

func (f *DispatchFace) SetFaceID(id uint64) { f.faceID = id }
func (f *DispatchFace) FaceID() uint64      { return f.faceID }
func (f *DispatchFace) LocalURI() *defn.URI  { return f.localURI }
func (f *DispatchFace) RemoteURI() *defn.URI { return f.remoteURI }
func (f *DispatchFace) Scope() defn.Scope    { return f.scope }
func (f *DispatchFace) LinkType() defn.LinkType { return f.linkType }
func (f *DispatchFace) MTU() int             { return defn.MaxNDNPacketSize }
func (f *DispatchFace) State() defn.State    { return f.state }

// SendPacket is called by fw.Thread when it wants to send a packet out this face.
// It encodes the packet as an LP frame (with PIT token if present) and invokes
// the send callback.
func (f *DispatchFace) SendPacket(out dispatch.OutPkt) {
	if f.onSend == nil || f.state != defn.Up {
		return
	}
	pkt := out.Pkt
	if pkt == nil || pkt.Raw == nil {
		return
	}

	// Wrap in LP frame with PIT token (like the real link service)
	lpFrag := &defn.FwLpPacket{
		Fragment: pkt.Raw,
		PitToken: out.PitToken,
	}
	lpPkt := defn.FwPacket{LpPacket: lpFrag}
	wire := lpPkt.Encode()
	if wire == nil {
		// Fallback: send raw bytes without LP envelope
		f.onSend(f.faceID, pkt.Raw.Join())
		return
	}
	f.onSend(f.faceID, wire.Join())
}

// SetState changes the face state.
func (f *DispatchFace) SetState(s defn.State) {
	f.state = s
}
