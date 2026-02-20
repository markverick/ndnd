package mgmt_2022

import (
	"fmt"

	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/ndn"
)

type MgmtConfig struct {
	// local means whether management service is of localhost
	local bool
	// signer is the signer used to sign the command
	signer ndn.Signer
	// spec is the NDN spec used to make Interests
	spec ndn.Spec
}

// MakeCmd makes and encodes a management command Interest for an explicit service.
func (mgmt *MgmtConfig) MakeCmd(service string, module string, cmd string,
	args *ControlArgs, config *ndn.InterestConfig) (*ndn.EncodedInterest, error) {

	params := ControlParameters{Val: args}

	var name enc.Name
	if mgmt.local {
		name = enc.Name{enc.LOCALHOST}
	} else {
		name = enc.Name{enc.LOCALHOP}
	}

	name = append(name,
		enc.NewGenericComponent(service),
		enc.NewGenericComponent(module),
		enc.NewGenericComponent(cmd),
		enc.NewGenericBytesComponent(params.Bytes()),
	)

	// Make and sign Interest
	return mgmt.spec.MakeInterest(name, config, enc.Wire{}, mgmt.signer)
}

// MakeCmdDict is the same as MakeCmd but receives a map[string]any as arguments.
func (mgmt *MgmtConfig) MakeCmdDict(service string, module string, cmd string, args map[string]any,
	config *ndn.InterestConfig) (*ndn.EncodedInterest, error) {
	// Parse arguments
	vv, err := DictToControlArgs(args)
	if err != nil {
		return nil, err
	}
	return mgmt.MakeCmd(service, module, cmd, vv, config)
}

// (AI GENERATED DESCRIPTION): Sets the Signer used by MgmtConfig for signing management packets.
func (mgmt *MgmtConfig) SetSigner(signer ndn.Signer) {
	mgmt.signer = signer
}

// (AI GENERATED DESCRIPTION): Creates a new MgmtConfig with the given local flag, signer, and spec, returning nil if either signer or spec is nil.
func NewConfig(local bool, signer ndn.Signer, spec ndn.Spec) *MgmtConfig {
	if signer == nil || spec == nil {
		return nil
	}
	return &MgmtConfig{
		local:  local,
		signer: signer,
		spec:   spec,
	}
}

// ExecServiceCmd builds and executes a management command for a service, then parses the ControlResponse.
func ExecServiceCmd(
	engine ndn.Engine,
	local bool,
	service string,
	module string,
	cmd string,
	args *ControlArgs,
	config *ndn.InterestConfig,
	signer ndn.Signer,
	checker ndn.SigChecker,
) (*ControlResponse, error) {
	mgmtCfg := NewConfig(local, signer, engine.Spec())
	if mgmtCfg == nil {
		return nil, fmt.Errorf("invalid management config")
	}

	interest, err := mgmtCfg.MakeCmd(service, module, cmd, args, config)
	if err != nil {
		return nil, err
	}
	return ExpressCmd(engine, interest, checker)
}

// ExpressCmd executes a management Interest and returns the parsed ControlResponse.
func ExpressCmd(engine ndn.Engine, interest *ndn.EncodedInterest, checker ndn.SigChecker) (*ControlResponse, error) {
	ch := make(chan struct {
		val *ControlResponse
		err error
	}, 1)
	if err := engine.Express(interest, func(args ndn.ExpressCallbackArgs) {
		resp, err := ParseCmdResponse(args, checker)
		ch <- struct {
			val *ControlResponse
			err error
		}{val: resp, err: err}
	}); err != nil {
		return nil, err
	}
	resp := <-ch
	return resp.val, resp.err
}

// ParseCmdResponse converts an express callback result into a management ControlResponse.
func ParseCmdResponse(args ndn.ExpressCallbackArgs, checker ndn.SigChecker) (*ControlResponse, error) {
	switch args.Result {
	case ndn.InterestResultNack:
		return nil, fmt.Errorf("nack received: %v", args.NackReason)
	case ndn.InterestResultTimeout:
		return nil, ndn.ErrDeadlineExceed
	case ndn.InterestResultError:
		return nil, args.Error
	case ndn.InterestResultData:
		data := args.Data
		if checker != nil && !checker(data.Name(), args.SigCovered, data.Signature()) {
			return nil, fmt.Errorf("command signature is not valid")
		}

		resp, err := ParseControlResponse(enc.NewWireView(data.Content()), true)
		if err != nil {
			return nil, err
		}
		if resp == nil || resp.Val == nil {
			return nil, fmt.Errorf("improper response")
		}
		if resp.Val.StatusCode != 200 {
			return resp, fmt.Errorf("command failed due to error %d: %s", resp.Val.StatusCode, resp.Val.StatusText)
		}
		return resp, nil
	default:
		return nil, fmt.Errorf("unknown result: %v", args.Result)
	}
}
