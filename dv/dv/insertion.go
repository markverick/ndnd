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

	object, _, err := spec.Spec{}.ReadData(enc.NewBufferView(paParams.Data))
	if err != nil {
		log.Warn(pfx, "Failed to parse Prefix Insertion inner data", "err", err)
		reply(resError)
		return
	}

	reply(pfx.onPrefixInsertionObject(object, args.IncomingFaceId.Unwrap()))
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

	// Backward compatibility: legacy packets carried ExpirationPeriod (0x6d).
	// We only preserve ExpirationPeriod=0 as an explicit withdraw signal.
	legacyExpiration, hasLegacyExpiration, err := parseLegacyExpirationPeriod(piWire)
	if err != nil {
		log.Warn(pfx, "Failed to parse legacy expiration period", "err", err)
		return resError
	}
	shouldWithdraw := hasLegacyExpiration && legacyExpiration == 0

	if !shouldWithdraw && params.ValidityPeriod != nil {
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

	if shouldWithdraw {
		pfx.Withdraw(prefix, faceId)
		return &mgmt.ControlResponse{
			Val: &mgmt.ControlResponseVal{
				StatusCode: 200,
				StatusText: "Prefix withdrawal command successful",
				Params: &mgmt.ControlArgs{
					Name:   prefix,
					FaceId: optional.Some(faceId),
				},
			},
		}
	}

	cost := params.Cost.GetOr(0)
	if cost > config.CostInfinity {
		log.Warn(pfx, "Invalid Cost value", "Cost", cost)
		return resError
	}

	pfx.AnnounceWithValidity(prefix, faceId, cost, params.ValidityPeriod)

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

func parseLegacyExpirationPeriod(piWire enc.Wire) (expiration uint64, has bool, err error) {
	reader := enc.NewWireView(piWire)
	for !reader.IsEOF() {
		typ, err := reader.ReadTLNum()
		if err != nil {
			return 0, false, err
		}
		l, err := reader.ReadTLNum()
		if err != nil {
			return 0, false, err
		}

		if typ != 0x6d {
			if err := reader.Skip(int(l)); err != nil {
				return 0, false, err
			}
			continue
		}

		buf, err := reader.ReadBuf(int(l))
		if err != nil {
			return 0, false, err
		}
		nat, _, err := enc.ParseNat(buf)
		if err != nil {
			return 0, false, err
		}
		return uint64(nat), true, nil
	}

	return 0, false, nil
}
