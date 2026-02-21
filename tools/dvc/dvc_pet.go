package dvc

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/ndn"
	mgmt "github.com/named-data/ndnd/std/ndn/mgmt_2022"
	"github.com/named-data/ndnd/std/object"
	"github.com/named-data/ndnd/std/types/optional"
)

func (t *Tool) preprocessPetArg(key string, val string) (string, string) {
	if key != "face" || !strings.Contains(val, "://") {
		return key, val
	}

	faceID, err := t.resolveFaceID(val)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(9)
	}

	return key, fmt.Sprintf("%d", faceID)
}

func (t *Tool) resolveFaceID(uri string) (uint64, error) {
	filter := mgmt.FaceQueryFilter{
		Val: &mgmt.FaceQueryFilterValue{Uri: optional.Some(uri)},
	}

	dataset, err := t.fetchNfdStatusDataset(enc.Name{
		enc.NewGenericComponent("faces"),
		enc.NewGenericComponent("query"),
		enc.NewGenericBytesComponent(filter.Encode().Join()),
	})
	if dataset == nil {
		return 0, fmt.Errorf("error fetching face status dataset: %+v", err)
	}

	status, err := mgmt.ParseFaceStatusMsg(enc.NewWireView(dataset), true)
	if err != nil {
		return 0, fmt.Errorf("error parsing face status: %+v", err)
	}

	if len(status.Vals) == 0 {
		return 0, fmt.Errorf("face not found for URI: %s", uri)
	}
	if len(status.Vals) > 1 {
		return 0, fmt.Errorf("multiple faces found for URI: %s", uri)
	}

	return status.Vals[0].FaceId, nil
}

func (t *Tool) fetchNfdStatusDataset(suffix enc.Name) (enc.Wire, error) {
	client := object.NewClient(t.engine, nil, nil)
	client.Start()
	defer client.Stop()

	ch := make(chan ndn.ConsumeState)
	client.ConsumeExt(ndn.ConsumeExtArgs{
		Name:       t.nfdPrefix().Append(suffix...),
		NoMetadata: true,
		Callback:   func(status ndn.ConsumeState) { ch <- status },
	})

	state := <-ch
	if err := state.Error(); err != nil {
		return nil, err
	}

	return state.Content(), nil
}

func (t *Tool) nfdPrefix() enc.Name {
	return enc.Name{
		enc.LOCALHOST,
		enc.NewGenericComponent("nfd"),
	}
}

func (t *Tool) convPetArg(ctrlArgs *mgmt.ControlArgs, key string, val string) {
	parseUint := func(v string) uint64 {
		parsed, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid value for %s: %s\n", key, v)
			os.Exit(9)
		}
		return parsed
	}

	parseName := func(v string) enc.Name {
		name, err := enc.NameFromStr(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid name for %s: %s\n", key, v)
			os.Exit(9)
		}
		return name
	}

	switch key {
	case "prefix":
		ctrlArgs.Name = parseName(val)
	case "face":
		ctrlArgs.FaceId = optional.Some(parseUint(val))
	case "cost":
		ctrlArgs.Cost = optional.Some(parseUint(val))
	case "expires":
		ctrlArgs.ExpirationPeriod = optional.Some(parseUint(val))
	default:
		fmt.Fprintf(os.Stderr, "Unknown command argument key: %s\n", key)
		os.Exit(9)
	}
}

func (t *Tool) printCtrlResponse(res *mgmt.ControlResponse) {
	if res == nil || res.Val == nil {
		return
	}
	fmt.Printf("Status=%d (%s)\n", res.Val.StatusCode, res.Val.StatusText)

	if res.Val.Params == nil {
		return
	}

	params := res.Val.Params.ToDict()
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		val := params[key]
		switch key {
		case "Origin":
			val = mgmt.RouteOrigin(val.(uint64)).String()
		}
		fmt.Printf("  %s=%v\n", key, val)
	}
}
