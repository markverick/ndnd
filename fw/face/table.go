/* YaNFD - Yet another NDN Forwarding Daemon
 *
 * Copyright (C) 2020-2021 Eric Newberry.
 *
 * This file is licensed under the terms of the MIT License, as found in LICENSE.md.
 */

package face

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/named-data/ndnd/fw/core"
	defn "github.com/named-data/ndnd/fw/defn"
	"github.com/named-data/ndnd/fw/dispatch"
	"github.com/named-data/ndnd/fw/table"
)

// FaceTable is the global face table for this forwarder
var FaceTable Table

// Table hold all faces used by the forwarder.
type Table struct {
	faces              sync.Map
	nextFaceID         atomic.Uint64 // starts at 1
	eventSubscriptions sync.Map      // map[uint64]func(FaceEvent)
	nextSubscriptionID atomic.Uint64
}

// (AI GENERATED DESCRIPTION): Implements the fmt.Stringer interface for Table, returning the literal string “face-table” as its textual identifier.
func (t *Table) String() string {
	return "face-table"
}

// Add adds a face to the face table.
func (t *Table) Add(face LinkService) {
	faceID := t.nextFaceID.Add(1) - 1
	face.SetFaceID(faceID)
	t.faces.Store(faceID, face)
	dispatch.AddFace(faceID, face)
	t.emitEvent(FaceEvent{
		Kind:   FaceEventCreated,
		FaceID: faceID,
		Face:   face,
	})
	core.Log.Debug(t, "Registered face", "faceid", faceID)
}

// Get gets the face with the specified ID (if any) from the face table.
func (t *Table) Get(id uint64) LinkService {
	face, ok := t.faces.Load(id)

	if ok {
		return face.(LinkService)
	}
	return nil
}

// GetByURI gets the face with the specified remote URI (if any) from the face table.
func (t *Table) GetByURI(remoteURI *defn.URI) LinkService {
	var found LinkService
	t.faces.Range(func(_, face interface{}) bool {
		if face.(LinkService).RemoteURI().String() == remoteURI.String() {
			found = face.(LinkService)
			return false
		}
		return true
	})
	return found
}

// GetAll returns points to all faces.
func (t *Table) GetAll() []LinkService {
	faces := make([]LinkService, 0)
	t.faces.Range(func(_, face interface{}) bool {
		faces = append(faces, face.(LinkService))
		return true
	})
	return faces
}

// Remove removes a face from the face table.
func (t *Table) Remove(id uint64) {
	selectedFace := t.Get(id)

	t.faces.Delete(id)
	dispatch.RemoveFace(id)
	table.Rib.CleanUpFace(id)
	table.Pet.CleanUpFace(id)
	if selectedFace != nil {
		t.emitEvent(FaceEvent{
			Kind:   FaceEventDestroyed,
			FaceID: id,
			Face:   selectedFace,
		})
	}
	core.Log.Info(t, "Unregistered face", "faceid", id)
}

// SubscribeEvents subscribes to face lifecycle events.
// The returned function unsubscribes when called.
func (t *Table) SubscribeEvents(callback func(FaceEvent)) func() {
	if callback == nil {
		return func() {}
	}

	subscriptionID := t.nextSubscriptionID.Add(1)
	t.eventSubscriptions.Store(subscriptionID, callback)

	return func() {
		t.eventSubscriptions.Delete(subscriptionID)
	}
}

func (t *Table) emitEvent(event FaceEvent) {
	t.eventSubscriptions.Range(func(_, value interface{}) bool {
		callback, ok := value.(func(FaceEvent))
		if !ok || callback == nil {
			return true
		}

		defer func() {
			if recovered := recover(); recovered != nil {
				core.Log.Error(t, "face event subscription panicked", "recovered", recovered)
			}
		}()
		callback(event)
		return true
	})
}

// expirationHandler stops the faces that have expired
// Runs in a separate goroutine called from Initialize()
func (t *Table) expirationHandler() {
	for !core.ShouldQuit {
		// Check for expired faces every 10 seconds
		time.Sleep(10 * time.Second)

		// Iterate the face table
		t.faces.Range(func(_, face interface{}) bool {
			transport := face.(LinkService).Transport()
			if transport != nil && transport.ExpirationPeriod() < 0 {
				core.Log.Info(t, "Face expired", "transport", transport)
				transport.Close()
			}
			return true
		})
	}
}
