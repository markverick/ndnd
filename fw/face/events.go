/* YaNFD - Yet another NDN Forwarding Daemon
 *
 * Copyright (C) 2020-2026 Eric Newberry, Tianyuan Yu.
 *
 * This file is licensed under the terms of the MIT License, as found in LICENSE.md.
 */

package face

import "fmt"

// FaceEventKind indicates the type of a face lifecycle event.
type FaceEventKind uint64

const (
	FaceEventCreated FaceEventKind = iota + 1
	FaceEventDestroyed
	FaceEventUp
	FaceEventDown
)

// FaceEvent reports a lifecycle update for a face.
type FaceEvent struct {
	Kind   FaceEventKind
	FaceID uint64
	Face   LinkService
}

func (k FaceEventKind) String() string {
	switch k {
	case FaceEventCreated:
		return "created"
	case FaceEventDestroyed:
		return "destroyed"
	case FaceEventUp:
		return "up"
	case FaceEventDown:
		return "down"
	default:
		return fmt.Sprintf("unknown(%d)", k)
	}
}
