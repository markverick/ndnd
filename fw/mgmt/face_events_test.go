package mgmt

import (
	"testing"

	enc "github.com/named-data/ndnd/std/encoding"
	mgmt "github.com/named-data/ndnd/std/ndn/mgmt_2022"
	"github.com/stretchr/testify/require"
)

func TestSelectFaceEventLatest(t *testing.T) {
	module := &FaceModule{
		eventCache: make(map[uint64]*mgmt.FaceEventNotification),
	}

	first := &mgmt.FaceEventNotification{Val: &mgmt.FaceEventNotificationValue{FaceEventKind: mgmt.FaceEventCreated, FaceId: 1}}
	second := &mgmt.FaceEventNotification{Val: &mgmt.FaceEventNotificationValue{FaceEventKind: mgmt.FaceEventDestroyed, FaceId: 1}}

	seq1 := module.cacheFaceEvent(first)
	seq2 := module.cacheFaceEvent(second)

	baseName := LOCAL_PREFIX.Append(
		enc.NewGenericComponent("faces"),
		enc.NewGenericComponent("events"),
	)
	seq, notification := module.selectFaceEvent(baseName, baseName, true)

	require.Equal(t, seq2, seq)
	require.NotEqual(t, seq1, seq)
	require.Equal(t, second, notification)
}

func TestSelectFaceEventBySequence(t *testing.T) {
	module := &FaceModule{
		eventCache: make(map[uint64]*mgmt.FaceEventNotification),
	}

	first := &mgmt.FaceEventNotification{Val: &mgmt.FaceEventNotificationValue{FaceEventKind: mgmt.FaceEventCreated, FaceId: 1}}
	second := &mgmt.FaceEventNotification{Val: &mgmt.FaceEventNotificationValue{FaceEventKind: mgmt.FaceEventUp, FaceId: 1}}

	seq1 := module.cacheFaceEvent(first)
	seq2 := module.cacheFaceEvent(second)

	baseName := LOCAL_PREFIX.Append(
		enc.NewGenericComponent("faces"),
		enc.NewGenericComponent("events"),
	)

	seq, notification := module.selectFaceEvent(baseName, baseName.Append(enc.NewSequenceNumComponent(seq1)), true)
	require.Equal(t, seq1, seq)
	require.Equal(t, first, notification)

	seq, notification = module.selectFaceEvent(baseName, baseName.Append(enc.NewSequenceNumComponent(seq2)), true)
	require.Equal(t, seq2, seq)
	require.Equal(t, second, notification)
}

func TestSelectFaceEventPrefixAdvance(t *testing.T) {
	module := &FaceModule{
		eventCache: make(map[uint64]*mgmt.FaceEventNotification),
	}

	first := &mgmt.FaceEventNotification{Val: &mgmt.FaceEventNotificationValue{FaceEventKind: mgmt.FaceEventCreated, FaceId: 1}}
	second := &mgmt.FaceEventNotification{Val: &mgmt.FaceEventNotificationValue{FaceEventKind: mgmt.FaceEventDown, FaceId: 1}}

	seq1 := module.cacheFaceEvent(first)
	seq2 := module.cacheFaceEvent(second)

	baseName := LOCAL_PREFIX.Append(
		enc.NewGenericComponent("faces"),
		enc.NewGenericComponent("events"),
	)

	seq, notification := module.selectFaceEvent(baseName, baseName.Append(enc.NewSequenceNumComponent(0)), true)
	require.Equal(t, seq1, seq)
	require.Equal(t, first, notification)

	seq, notification = module.selectFaceEvent(baseName, baseName.Append(enc.NewSequenceNumComponent(0)), false)
	require.Equal(t, uint64(0), seq)
	require.Nil(t, notification)

	seq, notification = module.selectFaceEvent(baseName, baseName.Append(enc.NewSequenceNumComponent(seq2+10)), true)
	require.Equal(t, uint64(0), seq)
	require.Nil(t, notification)
}
