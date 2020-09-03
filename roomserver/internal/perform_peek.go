// Copyright 2020 New Vector Ltd
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	fsAPI "github.com/matrix-org/dendrite/federationsender/api"
	"github.com/matrix-org/dendrite/roomserver/api"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/util"
	"github.com/sirupsen/logrus"
)

// PerformPeek handles peeking into matrix rooms, including over federation by talking to the federationsender.
func (r *RoomserverInternalAPI) PerformPeek(
	ctx context.Context,
	req *api.PerformPeekRequest,
	res *api.PerformPeekResponse,
) {
	roomID, err := r.performPeek(ctx, req)
	if err != nil {
		perr, ok := err.(*api.PerformError)
		if ok {
			res.Error = perr
		} else {
			res.Error = &api.PerformError{
				Msg: err.Error(),
			}
		}
	}
	res.RoomID = roomID
}

func (r *RoomserverInternalAPI) performPeek(
	ctx context.Context,
	req *api.PerformPeekRequest,
) (string, error) {
	// FIXME: there's way too much duplication with performJoin
	_, domain, err := gomatrixserverlib.SplitID('@', req.UserID)
	if err != nil {
		return "", &api.PerformError{
			Code: api.PerformErrorBadRequest,
			Msg:  fmt.Sprintf("Supplied user ID %q in incorrect format", req.UserID),
		}
	}
	if domain != r.Cfg.Matrix.ServerName {
		return "", &api.PerformError{
			Code: api.PerformErrorBadRequest,
			Msg:  fmt.Sprintf("User %q does not belong to this homeserver", req.UserID),
		}
	}
	if strings.HasPrefix(req.RoomIDOrAlias, "!") {
		return r.performPeekRoomByID(ctx, req)
	}
	if strings.HasPrefix(req.RoomIDOrAlias, "#") {
		return r.performPeekRoomByAlias(ctx, req)
	}
	return "", &api.PerformError{
		Code: api.PerformErrorBadRequest,
		Msg:  fmt.Sprintf("Room ID or alias %q is invalid", req.RoomIDOrAlias),
	}
}

func (r *RoomserverInternalAPI) performPeekRoomByAlias(
	ctx context.Context,
	req *api.PerformPeekRequest,
) (string, error) {
	// Get the domain part of the room alias.
	_, domain, err := gomatrixserverlib.SplitID('#', req.RoomIDOrAlias)
	if err != nil {
		return "", fmt.Errorf("Alias %q is not in the correct format", req.RoomIDOrAlias)
	}
	req.ServerNames = append(req.ServerNames, domain)

	// Check if this alias matches our own server configuration. If it
	// doesn't then we'll need to try a federated peek.
	var roomID string
	if domain != r.Cfg.Matrix.ServerName {
		// The alias isn't owned by us, so we will need to try peeking using
		// a remote server.
		dirReq := fsAPI.PerformDirectoryLookupRequest{
			RoomAlias:  req.RoomIDOrAlias, // the room alias to lookup
			ServerName: domain,            // the server to ask
		}
		dirRes := fsAPI.PerformDirectoryLookupResponse{}
		err = r.fsAPI.PerformDirectoryLookup(ctx, &dirReq, &dirRes)
		if err != nil {
			logrus.WithError(err).Errorf("error looking up alias %q", req.RoomIDOrAlias)
			return "", fmt.Errorf("Looking up alias %q over federation failed: %w", req.RoomIDOrAlias, err)
		}
		roomID = dirRes.RoomID
		req.ServerNames = append(req.ServerNames, dirRes.ServerNames...)
	} else {
		// Otherwise, look up if we know this room alias locally.
		roomID, err = r.DB.GetRoomIDForAlias(ctx, req.RoomIDOrAlias)
		if err != nil {
			return "", fmt.Errorf("Lookup room alias %q failed: %w", req.RoomIDOrAlias, err)
		}
	}

	// If the room ID is empty then we failed to look up the alias.
	if roomID == "" {
		return "", fmt.Errorf("Alias %q not found", req.RoomIDOrAlias)
	}

	// If we do, then pluck out the room ID and continue the peek.
	req.RoomIDOrAlias = roomID
	return r.performPeekRoomByID(ctx, req)
}

func (r *RoomserverInternalAPI) performPeekRoomByID(
	ctx context.Context,
	req *api.PerformPeekRequest,
) (roomID string, err error) {
	roomID = req.RoomIDOrAlias

	// Get the domain part of the room ID.
	_, domain, err := gomatrixserverlib.SplitID('!', roomID)
	if err != nil {
		return "", &api.PerformError{
			Code: api.PerformErrorBadRequest,
			Msg:  fmt.Sprintf("Room ID %q is invalid: %s", roomID, err),
		}
	}

	// If the server name in the room ID isn't ours then it's a
	// possible candidate for finding the room via federation. Add
	// it to the list of servers to try.
	if domain != r.Cfg.Matrix.ServerName {
		req.ServerNames = append(req.ServerNames, domain)
	}

	// If this room isn't world_readable, we reject.
	// XXX: would be nicer to call this with NIDs
	// XXX: we should probably factor out history_visibility checks into a common utility method somewhere
	// which handles the default value etc.
	var worldReadable = false
	ev, err := r.DB.GetStateEvent(ctx, roomID, "m.room.history_visibility", "")
	if ev != nil {
		content := map[string]string{}
		if err = json.Unmarshal(ev.Content(), &content); err != nil {
			util.GetLogger(ctx).WithError(err).Error("json.Unmarshal for history visibility failed")
			return
		}
		if visibility, ok := content["history_visibility"]; ok {
			worldReadable = visibility == "world_readable"
		}
	}

	if !worldReadable {
		return "", &api.PerformError{
			Code: api.PerformErrorNotAllowed,
			Msg: "Room is not world-readable",
		}
	}

	// TODO: handle federated peeks

	err = r.WriteOutputEvents(roomID, []api.OutputEvent{
		{
			Type: api.OutputTypeNewPeek,
			NewPeek: &api.OutputNewPeek{
				RoomID: roomID,
				UserID: req.UserID,
				DeviceID: req.DeviceID,
			},
		},
	})
	if err != nil {
		return
	}

	// By this point, if req.RoomIDOrAlias contained an alias, then
	// it will have been overwritten with a room ID by performPeekRoomByAlias.
	// We should now include this in the response so that the CS API can
	// return the right room ID.
	return roomID, nil;
}
