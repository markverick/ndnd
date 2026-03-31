/* YaNFD - Yet another NDN Forwarding Daemon
 *
 * Copyright (C) 2026
 *
 * This file is licensed under the terms of the MIT License, as found in LICENSE.md.
 */

package mgmt

import (
	"github.com/named-data/ndnd/fw/core"
	"github.com/named-data/ndnd/fw/defn"
	"github.com/named-data/ndnd/fw/table"
	enc "github.com/named-data/ndnd/std/encoding"
	mgmt "github.com/named-data/ndnd/std/ndn/mgmt_2022"
)

// MulticastStrategyChoiceModule handles multicast strategy choice management.
type MulticastStrategyChoiceModule struct {
	manager *Thread
}

// (AI GENERATED DESCRIPTION): Returns the identifier string "mgmt-multicast-strategy" for the module.
func (s *MulticastStrategyChoiceModule) String() string {
	return "mgmt-multicast-strategy"
}

// (AI GENERATED DESCRIPTION): Registers the specified manager by assigning it to the module's manager field.
func (s *MulticastStrategyChoiceModule) registerManager(manager *Thread) {
	s.manager = manager
}

// (AI GENERATED DESCRIPTION): Returns the manager thread associated with this module.
func (s *MulticastStrategyChoiceModule) getManager() *Thread {
	return s.manager
}

// (AI GENERATED DESCRIPTION): Handles incoming multicast strategy management Interests.
func (s *MulticastStrategyChoiceModule) handleIncomingInterest(interest *Interest) {
	// Only allow from /localhost
	if !LOCAL_PREFIX.IsPrefix(interest.Name()) {
		core.Log.Warn(s, "Received multicast strategy management Interest from non-local source - DROP")
		return
	}

	verb := interest.Name()[len(LOCAL_PREFIX)+1].String()
	switch verb {
	case "set":
		s.set(interest)
	case "unset":
		s.unset(interest)
	case "list":
		s.list(interest)
	default:
		s.manager.sendCtrlResp(interest, 501, "Unknown verb", nil)
		return
	}
}

func normalizeMulticastStrategy(name enc.Name) (enc.Name, bool) {
	if !defn.STRATEGY_PREFIX.IsPrefix(name) {
		return nil, false
	}

	if len(name) == len(defn.STRATEGY_PREFIX)+1 {
		strategyName := name[len(defn.STRATEGY_PREFIX)].String()
		switch strategyName {
		case "broadcast":
			return defn.BROADCAST_STRATEGY, true
		case "bier":
			return defn.BIER_STRATEGY, true
		default:
			return nil, false
		}
	}

	if len(name) == len(defn.STRATEGY_PREFIX)+2 && name[len(defn.STRATEGY_PREFIX)+1].IsVersion() {
		strategyName := name[len(defn.STRATEGY_PREFIX)].String()
		versionBytes := name[len(defn.STRATEGY_PREFIX)+1].Val
		version, _, err := enc.ParseNat(versionBytes)
		if err != nil || version != 1 {
			return nil, false
		}
		switch strategyName {
		case "broadcast":
			return defn.BROADCAST_STRATEGY, true
		case "bier":
			return defn.BIER_STRATEGY, true
		default:
			return nil, false
		}
	}

	return nil, false
}

// (AI GENERATED DESCRIPTION): Handles a SetStrategy control request for multicast strategies.
func (s *MulticastStrategyChoiceModule) set(interest *Interest) {
	if len(interest.Name()) < len(LOCAL_PREFIX)+3 {
		s.manager.sendCtrlResp(interest, 400, "ControlParameters is incorrect", nil)
		return
	}

	params := decodeControlParameters(s, interest)
	if params == nil {
		s.manager.sendCtrlResp(interest, 400, "ControlParameters is incorrect", nil)
		return
	}

	if params.Name == nil {
		s.manager.sendCtrlResp(interest, 400, "ControlParameters is incorrect (missing Name)", nil)
		return
	}

	if params.Strategy == nil {
		s.manager.sendCtrlResp(interest, 400, "ControlParameters is incorrect (missing Strategy)", nil)
		return
	}

	normalized, ok := normalizeMulticastStrategy(params.Strategy.Name)
	if !ok {
		core.Log.Warn(s, "Invalid multicast strategy", "strategy", params.Strategy.Name)
		s.manager.sendCtrlResp(interest, 404, "Invalid multicast strategy", nil)
		return
	}

	table.MulticastStrategyTable.SetStrategyEnc(params.Name, normalized)

	s.manager.sendCtrlResp(interest, 200, "OK", &mgmt.ControlArgs{
		Name:     params.Name,
		Strategy: &mgmt.Strategy{Name: normalized},
	})

	core.Log.Info(s, "Set multicast strategy", "name", params.Name, "strategy", normalized)
}

// (AI GENERATED DESCRIPTION): Unsets a multicast strategy for a given name.
func (s *MulticastStrategyChoiceModule) unset(interest *Interest) {
	if len(interest.Name()) < len(LOCAL_PREFIX)+3 {
		s.manager.sendCtrlResp(interest, 400, "ControlParameters is incorrect", nil)
		return
	}

	params := decodeControlParameters(s, interest)
	if params == nil {
		s.manager.sendCtrlResp(interest, 400, "ControlParameters is incorrect", nil)
		return
	}

	if params.Name == nil {
		s.manager.sendCtrlResp(interest, 400, "ControlParameters is incorrect (missing Name)", nil)
		return
	}

	if len(params.Name) == 0 {
		s.manager.sendCtrlResp(interest, 400, "ControlParameters is incorrect (empty Name)", nil)
		return
	}

	table.MulticastStrategyTable.UnSetStrategyEnc(params.Name)
	core.Log.Info(s, "Unset multicast strategy", "name", params.Name)

	s.manager.sendCtrlResp(interest, 200, "OK", &mgmt.ControlArgs{Name: params.Name})
}

// (AI GENERATED DESCRIPTION): Handles listing multicast strategies.
func (s *MulticastStrategyChoiceModule) list(interest *Interest) {
	if len(interest.Name()) > len(LOCAL_PREFIX)+2 {
		// Ignore because contains version and/or segment components
		return
	}

	entries := table.MulticastStrategyTable.GetAllForwardingStrategies()
	choices := []*mgmt.StrategyChoice{}
	for _, fsEntry := range entries {
		choices = append(choices, &mgmt.StrategyChoice{
			Name:     fsEntry.Name(),
			Strategy: &mgmt.Strategy{Name: fsEntry.GetStrategy()},
		})
	}
	dataset := &mgmt.StrategyChoiceMsg{StrategyChoices: choices}

	name := LOCAL_PREFIX.Append(
		enc.NewGenericComponent("multicast-strategy-choice"),
		enc.NewGenericComponent("list"),
	)
	s.manager.sendStatusDataset(interest, name, dataset.Encode())
}
