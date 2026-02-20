package object

import (
	"fmt"
	"time"

	dvtlv "github.com/named-data/ndnd/dv/tlv"
	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/ndn"
	mgmt "github.com/named-data/ndnd/std/ndn/mgmt_2022"
	spec "github.com/named-data/ndnd/std/ndn/spec_2022"
	"github.com/named-data/ndnd/std/types/optional"
	"github.com/named-data/ndnd/std/utils"
)

const (
	exposeLifetime            = 1 * time.Hour
	commandTimeout            = 1 * time.Second
	defaultGatewayInsertRoute = "/localhop/route/insert"
)

func (c *Client) insertPrefix(args ndn.Announcement, withdraw bool) error {
	insertPrefix, err := enc.NameFromStr(defaultGatewayInsertRoute)
	if err != nil {
		return fmt.Errorf("invalid gateway insert route %q: %w", defaultGatewayInsertRoute, err)
	}

	appParam, err := c.prefixInsertionAppParam(args, withdraw)
	if err != nil {
		return err
	}

	signer := c.SuggestSigner(insertPrefix)
	if signer == nil {
		// Some schemas only define rules for the PrefixAnnouncement object
		// namespace, not for /localhop/route/insert command interests.
		signer = c.SuggestSigner(args.Name.Append(enc.NewKeywordComponent("PA")))
		if signer == nil {
			return fmt.Errorf("no signer found for prefix insertion command: %s", insertPrefix)
		}
	}

	interest, err := c.engine.Spec().MakeInterest(
		insertPrefix,
		&ndn.InterestConfig{
			Lifetime:    optional.Some(commandTimeout),
			Nonce:       utils.ConvertNonce(c.engine.Timer().Nonce()),
			MustBeFresh: true,
			SigNonce:    c.engine.Timer().Nonce(),
			SigTime:     optional.Some(time.Duration(c.engine.Timer().Now().UnixMilli()) * time.Millisecond),
		},
		enc.Wire{appParam},
		signer,
	)
	if err != nil {
		return err
	}

	_, err = mgmt.ExpressCmd(c.engine, interest, nil)
	return err
}

func (c *Client) prefixInsertionAppParam(args ndn.Announcement, withdraw bool) ([]byte, error) {
	version := uint64(c.engine.Timer().Now().UnixMicro())

	objName := args.Name.Clone().
		Append(enc.NewKeywordComponent("PA")).
		Append(enc.NewVersionComponent(version)).
		Append(enc.NewSegmentComponent(0))

	signer := c.SuggestSigner(objName)
	if signer == nil {
		return nil, fmt.Errorf("no signer found for prefix insertion object: %s", objName)
	}

	obj, err := c.engine.Spec().MakeData(
		objName,
		&ndn.DataConfig{
			ContentType: optional.Some(ndn.ContentTypePrefixAnnouncement),
			Freshness:   optional.Some(exposeLifetime),
		},
		enc.Wire{c.prefixInsertionInnerContent(args, withdraw)},
		signer,
	)
	if err != nil {
		return nil, err
	}

	return (&dvtlv.PrefixInsertion{
		Data: obj.Wire.Join(),
	}).Bytes(), nil
}

func (c *Client) prefixInsertionInnerContent(args ndn.Announcement, withdraw bool) []byte {
	// Prefix withdraw is signaled through legacy expiration=0 for compatibility.
	if withdraw {
		return (&mgmt.ControlArgs{
			ExpirationPeriod: optional.Some(uint64(0)),
		}).Bytes()
	}

	content := &dvtlv.PrefixInsertionInnerContent{
		ValidityPeriod: &spec.ValidityPeriod{
			NotAfter: c.engine.Timer().Now().UTC().Add(exposeLifetime).Format(spec.TimeFmt),
		},
	}
	if args.Cost > 0 {
		content.Cost = optional.Some(args.Cost)
	}
	return content.Bytes()
}
