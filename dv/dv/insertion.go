package dv

import (
	"time"

	"github.com/named-data/ndnd/dv/config"
	"github.com/named-data/ndnd/dv/tlv"
	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/log"
	"github.com/named-data/ndnd/std/ndn"
	mgmt "github.com/named-data/ndnd/std/ndn/mgmt_2022"
	spec "github.com/named-data/ndnd/std/ndn/spec_2022"
	sec "github.com/named-data/ndnd/std/security"
	sig "github.com/named-data/ndnd/std/security/signer"
	"github.com/named-data/ndnd/std/types/optional"
)

func (pfx *PrefixModule) onInsertion(args ndn.InterestHandlerArgs) {
	resError := &mgmt.ControlResponse{
		Val: &mgmt.ControlResponseVal{
			StatusCode: 400,
			StatusText: "Failed to execute prefix insertion",
			Params:     nil,
		},
	}

	reply := func(res *mgmt.ControlResponse) {
		signer := sig.NewSha256Signer()
		data, err := spec.Spec{}.MakeData(
			args.Interest.Name(),
			&ndn.DataConfig{
				ContentType: optional.Some(ndn.ContentTypeBlob),
				Freshness:   optional.Some(1 * time.Second),
			},
			res.Encode(),
			signer,
		)
		if err != nil {
			log.Warn(pfx, "Failed to make Prefix Insertion response Data", "err", err)
			return
		}
		args.Reply(data.Wire)
	}

	// If there is no incoming face ID, we can't use this.
	if !args.IncomingFaceId.IsSet() {
		log.Warn(pfx, "Received Prefix Insertion with no incoming face ID, ignoring")
		reply(resError)
		return
	}

	// Check if app param is present.
	if args.Interest.AppParam() == nil {
		log.Warn(pfx, "Received Prefix Insertion with no AppParam, ignoring")
		reply(resError)
		return
	}

	paParams, err := tlv.ParsePrefixInsertion(enc.NewWireView(args.Interest.AppParam()), true)
	if err != nil {
		log.Warn(pfx, "Failed to parse Prefix Insertion AppParam", "err", err)
		reply(resError)
		return
	}
	if paParams.Data == nil {
		reply(resError)
		return
	}

	object, sigCov, err := spec.Spec{}.ReadData(enc.NewBufferView(paParams.Data))
	if err != nil {
		log.Warn(pfx, "Failed to parse Prefix Insertion inner data", "err", err)
		reply(resError)
		return
	}

	faceId := args.IncomingFaceId.Unwrap()
	if pfx.insertionTrust == nil {
		reply(pfx.onPrefixInsertionObject(object, faceId))
		return
	}

	pfx.validatePrefixAnnouncement(object, sigCov, args.IncomingFaceId, func(valid bool, err error) {
		if !valid || err != nil {
			log.Warn(pfx, "Prefix insertion signature validation failed",
				"name", object.Name(), "valid", valid, "err", err)
			reply(&mgmt.ControlResponse{
				Val: &mgmt.ControlResponseVal{
					StatusCode: 403,
					StatusText: "Prefix announcement signature validation failed",
					Params:     nil,
				},
			})
			return
		}
		reply(pfx.onPrefixInsertionObject(object, faceId))
	})
}

func (pfx *PrefixModule) onPrefixInsertionObject(object ndn.Data, faceId uint64) *mgmt.ControlResponse {
	resError := &mgmt.ControlResponse{
		Val: &mgmt.ControlResponseVal{
			StatusCode: 400,
			StatusText: "Failed to execute prefix insertion",
			Params:     nil,
		},
	}

	if contentType, set := object.ContentType().Get(); !set || contentType != ndn.ContentTypePrefixAnnouncement {
		log.Warn(pfx, "Prefix Announcement Object does not have the correct content type",
			"contentType", object.ContentType())
		return resError
	}

	var prefix enc.Name
	var version uint64
	found := false
	for i, c := range object.Name() {
		if !c.IsKeyword("PA") {
			continue
		}
		if len(object.Name()) != i+3 ||
			!object.Name().At(i+1).IsVersion() ||
			!object.Name().At(i+2).IsSegment() ||
			object.Name().At(i+2).NumberVal() != 0 {
			break
		}
		prefix = object.Name().Prefix(i)
		version = object.Name().At(i + 1).NumberVal()
		found = true
		break
	}

	if !found {
		log.Warn(pfx, "Prefix Announcement Object name not in correct format", "name", object.Name())
		return resError
	}

	// Check if we've seen a newer version of this prefix insertion.
	prefixStr := string(prefix.Bytes())
	pfx.mu.Lock()
	if lastVersion, exists := pfx.seenPrefixVersions[prefixStr]; exists && lastVersion >= version {
		pfx.mu.Unlock()
		log.Info(pfx, "Rejecting older or duplicate prefix insertion",
			"prefix", prefix,
			"version", version,
			"lastVersion", lastVersion)
		return &mgmt.ControlResponse{
			Val: &mgmt.ControlResponseVal{
				StatusCode: 409,
				StatusText: "Older or duplicate prefix insertion version",
				Params:     nil,
			},
		}
	}
	pfx.seenPrefixVersions[prefixStr] = version
	pfx.mu.Unlock()

	piWire := object.Content()
	params, err := tlv.ParsePrefixInsertionInnerContent(enc.NewWireView(piWire), true)
	if err != nil {
		log.Warn(pfx, "Failed to parse prefix announcement object", "err", err)
		return resError
	}

	if params.ValidityPeriod != nil {
		now := time.Now().UTC()
		if params.ValidityPeriod.NotBefore != "" {
			notBefore, err := time.Parse(spec.TimeFmt, params.ValidityPeriod.NotBefore)
			if err != nil {
				log.Warn(pfx, "Failed to parse NotBefore time", "err", err, "value", params.ValidityPeriod.NotBefore)
				return resError
			}
			if now.Before(notBefore) {
				log.Info(pfx, "Prefix insertion outside validity period",
					"prefix", prefix,
					"notBefore", notBefore,
					"now", now)
				return &mgmt.ControlResponse{
					Val: &mgmt.ControlResponseVal{
						StatusCode: 403,
						StatusText: "Prefix insertion outside validity period",
						Params:     nil,
					},
				}
			}
		}
		if params.ValidityPeriod.NotAfter != "" {
			notAfter, err := time.Parse(spec.TimeFmt, params.ValidityPeriod.NotAfter)
			if err != nil {
				log.Warn(pfx, "Failed to parse NotAfter time", "err", err, "value", params.ValidityPeriod.NotAfter)
				return resError
			}
			if now.After(notAfter) {
				log.Info(pfx, "Prefix insertion outside validity period",
					"prefix", prefix,
					"notAfter", notAfter,
					"now", now)
				return &mgmt.ControlResponse{
					Val: &mgmt.ControlResponseVal{
						StatusCode: 403,
						StatusText: "Prefix insertion outside validity period",
						Params:     nil,
					},
				}
			}
		}
	}

	cost := params.Cost.GetOr(0)
	if cost > config.CostInfinity {
		log.Warn(pfx, "Invalid Cost value", "Cost", cost)
		return resError
	}

	pfx.Announce(prefix, faceId, cost, cost == 1, params.ValidityPeriod)

	return &mgmt.ControlResponse{
		Val: &mgmt.ControlResponseVal{
			StatusCode: 200,
			StatusText: "Prefix insertion command successful",
			Params: &mgmt.ControlArgs{
				Name:   prefix,
				FaceId: optional.Some(faceId),
				Cost:   optional.Some(cost),
			},
		},
	}
}

func (pfx *PrefixModule) validatePrefixAnnouncement(
	object ndn.Data,
	sigCov enc.Wire,
	incomingFace optional.Optional[uint64],
	callback func(bool, error),
) {
	if pfx.insertionTrust == nil {
		callback(true, nil)
		return
	}

	overrideName := object.Name()
	if len(overrideName) >= 2 && overrideName.At(-1).IsSegment() && overrideName.At(-2).IsVersion() {
		overrideName = overrideName.Prefix(-2)
	}

	pfx.insertionTrust.Validate(sec.TrustConfigValidateArgs{
		Data:         object,
		DataSigCov:   sigCov,
		OverrideName: overrideName,
		Fetch: func(name enc.Name, cfg *ndn.InterestConfig, cb ndn.ExpressCallbackFunc) {
			if cfg == nil {
				cfg = &ndn.InterestConfig{}
			}
			cfg.NextHopId = incomingFace

			pfx.client.ExpressR(ndn.ExpressRArgs{
				Name:     name,
				Config:   cfg,
				Retries:  3,
				Callback: cb,
				TryStore: pfx.client.Store(),
			})
		},
		Callback: callback,
	})
}
