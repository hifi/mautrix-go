// Copyright (c) 2020 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package appservice

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type IntentAPI struct {
	*mautrix.Client
	bot       *mautrix.Client
	as        *AppService
	Localpart string
	UserID    id.UserID

	IsCustomPuppet bool
}

func (as *AppService) NewIntentAPI(localpart string) *IntentAPI {
	userID := id.NewUserID(localpart, as.HomeserverDomain)
	bot := as.BotClient()
	if userID == bot.UserID {
		bot = nil
	}
	return &IntentAPI{
		Client:    as.Client(userID),
		bot:       bot,
		as:        as,
		Localpart: localpart,
		UserID:    userID,

		IsCustomPuppet: false,
	}
}

func (intent *IntentAPI) Register() error {
	_, _, err := intent.Client.Register(&mautrix.ReqRegister{
		Username:     intent.Localpart,
		Type:         mautrix.AuthTypeAppservice,
		InhibitLogin: true,
	})
	return err
}

func (intent *IntentAPI) EnsureRegistered() error {
	if intent.IsCustomPuppet || intent.as.StateStore.IsRegistered(intent.UserID) {
		return nil
	}

	err := intent.Register()
	if err != nil && !errors.Is(err, mautrix.MUserInUse) {
		return fmt.Errorf("failed to ensure registered: %w", err)
	}
	intent.as.StateStore.MarkRegistered(intent.UserID)
	return nil
}

type EnsureJoinedParams struct {
	IgnoreCache bool
	BotOverride *mautrix.Client
}

func (intent *IntentAPI) EnsureJoined(roomID id.RoomID, extra ...EnsureJoinedParams) error {
	var params EnsureJoinedParams
	if len(extra) > 1 {
		panic("invalid number of extra parameters")
	} else if len(extra) == 1 {
		params = extra[0]
	}
	if intent.as.StateStore.IsInRoom(roomID, intent.UserID) && !params.IgnoreCache {
		return nil
	}

	if err := intent.EnsureRegistered(); err != nil {
		return fmt.Errorf("failed to ensure joined: %w", err)
	}

	resp, err := intent.JoinRoomByID(roomID)
	if err != nil {
		bot := intent.bot
		if params.BotOverride != nil {
			bot = params.BotOverride
		}
		if !errors.Is(err, mautrix.MForbidden) || bot == nil {
			return fmt.Errorf("failed to ensure joined: %w", err)
		}
		_, inviteErr := bot.InviteUser(roomID, &mautrix.ReqInviteUser{
			UserID: intent.UserID,
		})
		if inviteErr != nil {
			return fmt.Errorf("failed to invite in ensure joined: %w", inviteErr)
		}
		resp, err = intent.JoinRoomByID(roomID)
		if err != nil {
			return fmt.Errorf("failed to ensure joined after invite: %w", err)
		}
	}
	intent.as.StateStore.SetMembership(resp.RoomID, intent.UserID, event.MembershipJoin)
	return nil
}

func (intent *IntentAPI) SendMessageEvent(roomID id.RoomID, eventType event.Type, contentJSON interface{}) (*mautrix.RespSendEvent, error) {
	if err := intent.EnsureJoined(roomID); err != nil {
		return nil, err
	}
	return intent.Client.SendMessageEvent(roomID, eventType, contentJSON)
}

func (intent *IntentAPI) SendMassagedMessageEvent(roomID id.RoomID, eventType event.Type, contentJSON interface{}, ts int64) (*mautrix.RespSendEvent, error) {
	if err := intent.EnsureJoined(roomID); err != nil {
		return nil, err
	}
	return intent.Client.SendMessageEvent(roomID, eventType, contentJSON, mautrix.ReqSendEvent{Timestamp: ts})
}

func (intent *IntentAPI) updateStoreWithOutgoingEvent(roomID id.RoomID, eventType event.Type, stateKey string, contentJSON interface{}, eventID id.EventID) {
	fakeEvt := &event.Event{
		StateKey: &stateKey,
		Sender:   intent.UserID,
		Type:     eventType,
		ID:       eventID,
		RoomID:   roomID,
		Content:  event.Content{},
	}
	var err error
	fakeEvt.Content.VeryRaw, err = json.Marshal(contentJSON)
	if err != nil {
		intent.Logger.Debugfln("Failed to marshal state event content to update state store: %v", err)
		return
	}
	err = json.Unmarshal(fakeEvt.Content.VeryRaw, &fakeEvt.Content.Raw)
	if err != nil {
		intent.Logger.Debugfln("Failed to unmarshal state event content to update state store: %v", err)
		return
	}
	err = fakeEvt.Content.ParseRaw(fakeEvt.Type)
	if err != nil {
		intent.Logger.Debugfln("Failed to parse state event content to update state store: %v", err)
		return
	}
	intent.as.UpdateState(fakeEvt)
}

func (intent *IntentAPI) SendStateEvent(roomID id.RoomID, eventType event.Type, stateKey string, contentJSON interface{}) (*mautrix.RespSendEvent, error) {
	if err := intent.EnsureJoined(roomID); err != nil {
		return nil, err
	}
	resp, err := intent.Client.SendStateEvent(roomID, eventType, stateKey, contentJSON)
	if err == nil && resp != nil {
		intent.updateStoreWithOutgoingEvent(roomID, eventType, stateKey, contentJSON, resp.EventID)
	}
	return resp, err
}

func (intent *IntentAPI) SendMassagedStateEvent(roomID id.RoomID, eventType event.Type, stateKey string, contentJSON interface{}, ts int64) (*mautrix.RespSendEvent, error) {
	if err := intent.EnsureJoined(roomID); err != nil {
		return nil, err
	}
	resp, err := intent.Client.SendMassagedStateEvent(roomID, eventType, stateKey, contentJSON, ts)
	if err == nil && resp != nil {
		intent.updateStoreWithOutgoingEvent(roomID, eventType, stateKey, contentJSON, resp.EventID)
	}
	return resp, err
}

func (intent *IntentAPI) StateEvent(roomID id.RoomID, eventType event.Type, stateKey string, outContent interface{}) error {
	if err := intent.EnsureJoined(roomID); err != nil {
		return err
	}
	err := intent.Client.StateEvent(roomID, eventType, stateKey, outContent)
	if err == nil {
		intent.updateStoreWithOutgoingEvent(roomID, eventType, stateKey, outContent, "")
	}
	return err
}

func (intent *IntentAPI) State(roomID id.RoomID) (mautrix.RoomStateMap, error) {
	if err := intent.EnsureJoined(roomID); err != nil {
		return nil, err
	}
	state, err := intent.Client.State(roomID)
	if err == nil {
		for _, events := range state {
			for _, evt := range events {
				intent.as.UpdateState(evt)
			}
		}
	}
	return state, err
}

func (intent *IntentAPI) InviteUser(roomID id.RoomID, req *mautrix.ReqInviteUser) (resp *mautrix.RespInviteUser, err error) {
	resp, err = intent.Client.InviteUser(roomID, req)
	if err == nil {
		intent.as.StateStore.SetMembership(roomID, req.UserID, event.MembershipInvite)
	}
	return
}

func (intent *IntentAPI) KickUser(roomID id.RoomID, req *mautrix.ReqKickUser) (resp *mautrix.RespKickUser, err error) {
	resp, err = intent.Client.KickUser(roomID, req)
	if err == nil {
		intent.as.StateStore.SetMembership(roomID, req.UserID, event.MembershipLeave)
	}
	return
}

func (intent *IntentAPI) BanUser(roomID id.RoomID, req *mautrix.ReqBanUser) (resp *mautrix.RespBanUser, err error) {
	resp, err = intent.Client.BanUser(roomID, req)
	if err == nil {
		intent.as.StateStore.SetMembership(roomID, req.UserID, event.MembershipBan)
	}
	return
}

func (intent *IntentAPI) UnbanUser(roomID id.RoomID, req *mautrix.ReqUnbanUser) (resp *mautrix.RespUnbanUser, err error) {
	resp, err = intent.Client.UnbanUser(roomID, req)
	if err == nil {
		intent.as.StateStore.SetMembership(roomID, req.UserID, event.MembershipLeave)
	}
	return
}

func (intent *IntentAPI) Member(roomID id.RoomID, userID id.UserID) *event.MemberEventContent {
	member, ok := intent.as.StateStore.TryGetMember(roomID, userID)
	if !ok {
		_ = intent.StateEvent(roomID, event.StateMember, string(userID), &member)
		intent.as.StateStore.SetMember(roomID, userID, member)
	}
	return member
}

func (intent *IntentAPI) PowerLevels(roomID id.RoomID) (pl *event.PowerLevelsEventContent, err error) {
	pl = intent.as.StateStore.GetPowerLevels(roomID)
	if pl == nil {
		pl = &event.PowerLevelsEventContent{}
		err = intent.StateEvent(roomID, event.StatePowerLevels, "", pl)
		if err == nil {
			intent.as.StateStore.SetPowerLevels(roomID, pl)
		}
	}
	return
}

func (intent *IntentAPI) SetPowerLevels(roomID id.RoomID, levels *event.PowerLevelsEventContent) (resp *mautrix.RespSendEvent, err error) {
	resp, err = intent.SendStateEvent(roomID, event.StatePowerLevels, "", &levels)
	if err == nil {
		intent.as.StateStore.SetPowerLevels(roomID, levels)
	}
	return
}

func (intent *IntentAPI) SetPowerLevel(roomID id.RoomID, userID id.UserID, level int) (*mautrix.RespSendEvent, error) {
	pl, err := intent.PowerLevels(roomID)
	if err != nil {
		return nil, err
	}

	if pl.GetUserLevel(userID) != level {
		pl.SetUserLevel(userID, level)
		return intent.SendStateEvent(roomID, event.StatePowerLevels, "", &pl)
	}
	return nil, nil
}

func (intent *IntentAPI) UserTyping(roomID id.RoomID, typing bool, timeout int64) (resp *mautrix.RespTyping, err error) {
	if intent.as.StateStore.IsTyping(roomID, intent.UserID) == typing {
		return
	}
	resp, err = intent.Client.UserTyping(roomID, typing, timeout)
	if err != nil {
		return
	}
	if !typing {
		timeout = -1
	}
	intent.as.StateStore.SetTyping(roomID, intent.UserID, timeout)
	return
}

func (intent *IntentAPI) SendText(roomID id.RoomID, text string) (*mautrix.RespSendEvent, error) {
	if err := intent.EnsureJoined(roomID); err != nil {
		return nil, err
	}
	return intent.Client.SendText(roomID, text)
}

func (intent *IntentAPI) SendImage(roomID id.RoomID, body string, url id.ContentURI) (*mautrix.RespSendEvent, error) {
	if err := intent.EnsureJoined(roomID); err != nil {
		return nil, err
	}
	return intent.Client.SendImage(roomID, body, url)
}

func (intent *IntentAPI) SendVideo(roomID id.RoomID, body string, url id.ContentURI) (*mautrix.RespSendEvent, error) {
	if err := intent.EnsureJoined(roomID); err != nil {
		return nil, err
	}
	return intent.Client.SendVideo(roomID, body, url)
}

func (intent *IntentAPI) SendNotice(roomID id.RoomID, text string) (*mautrix.RespSendEvent, error) {
	if err := intent.EnsureJoined(roomID); err != nil {
		return nil, err
	}
	return intent.Client.SendNotice(roomID, text)
}

func (intent *IntentAPI) RedactEvent(roomID id.RoomID, eventID id.EventID, req ...mautrix.ReqRedact) (*mautrix.RespSendEvent, error) {
	if err := intent.EnsureJoined(roomID); err != nil {
		return nil, err
	}
	return intent.Client.RedactEvent(roomID, eventID, req...)
}

func (intent *IntentAPI) SetRoomName(roomID id.RoomID, roomName string) (*mautrix.RespSendEvent, error) {
	return intent.SendStateEvent(roomID, event.StateRoomName, "", map[string]interface{}{
		"name": roomName,
	})
}

func (intent *IntentAPI) SetRoomAvatar(roomID id.RoomID, avatarURL id.ContentURI) (*mautrix.RespSendEvent, error) {
	return intent.SendStateEvent(roomID, event.StateRoomAvatar, "", map[string]interface{}{
		"url": avatarURL.String(),
	})
}

func (intent *IntentAPI) SetRoomTopic(roomID id.RoomID, topic string) (*mautrix.RespSendEvent, error) {
	return intent.SendStateEvent(roomID, event.StateTopic, "", map[string]interface{}{
		"topic": topic,
	})
}

func (intent *IntentAPI) SetDisplayName(displayName string) error {
	if err := intent.EnsureRegistered(); err != nil {
		return err
	}
	return intent.Client.SetDisplayName(displayName)
}

func (intent *IntentAPI) SetAvatarURL(avatarURL id.ContentURI) error {
	if err := intent.EnsureRegistered(); err != nil {
		return err
	}
	return intent.Client.SetAvatarURL(avatarURL)
}

func (intent *IntentAPI) Whoami() (*mautrix.RespWhoami, error) {
	if err := intent.EnsureRegistered(); err != nil {
		return nil, err
	}
	return intent.Client.Whoami()
}

func (intent *IntentAPI) JoinedMembers(roomID id.RoomID) (resp *mautrix.RespJoinedMembers, err error) {
	resp, err = intent.Client.JoinedMembers(roomID)
	if err != nil {
		return
	}
	for userID, member := range resp.Joined {
		var displayname string
		var avatarURL id.ContentURIString
		if member.DisplayName != nil {
			displayname = *member.DisplayName
		}
		if member.AvatarURL != nil {
			avatarURL = id.ContentURIString(*member.AvatarURL)
		}
		intent.as.StateStore.SetMember(roomID, userID, &event.MemberEventContent{
			Membership:  event.MembershipJoin,
			AvatarURL:   avatarURL,
			Displayname: displayname,
		})
	}
	return
}

func (intent *IntentAPI) Members(roomID id.RoomID, req ...mautrix.ReqMembers) (resp *mautrix.RespMembers, err error) {
	resp, err = intent.Client.Members(roomID, req...)
	if err != nil {
		return
	}
	for _, evt := range resp.Chunk {
		intent.as.UpdateState(evt)
	}
	return
}

func (intent *IntentAPI) EnsureInvited(roomID id.RoomID, userID id.UserID) error {
	if !intent.as.StateStore.IsInvited(roomID, userID) {
		_, err := intent.InviteUser(roomID, &mautrix.ReqInviteUser{
			UserID: userID,
		})
		if httpErr, ok := err.(mautrix.HTTPError); ok && httpErr.RespError != nil && strings.Contains(httpErr.RespError.Err, "is already in the room") {
			return nil
		}
		return err
	}
	return nil
}
