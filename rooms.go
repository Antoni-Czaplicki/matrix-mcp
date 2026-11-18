package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var knownNetworks = []string{"messenger", "instagram", "whatsapp", "discord", "imessage", "signal", "telegram", "gmessages", "slack", "linkedin"}

type RoomInfo struct {
	ID          id.RoomID `json:"room_id"`
	Name        string    `json:"name"`
	Network     string    `json:"network,omitempty"`
	Kind        string    `json:"kind"` // dm, group, bridge_bot, space
	Topic       string    `json:"topic,omitempty"`
	Alias       string    `json:"alias,omitempty"`
	Members     int       `json:"members"`
	Encrypted   bool      `json:"encrypted"`
	Tags        []string  `json:"tags,omitempty"`
	LastTime    string    `json:"last_activity,omitempty"`
	LastSender  string    `json:"last_sender,omitempty"`
	LastPreview string    `json:"last_message,omitempty"`
	LastFromMe  bool      `json:"last_sender_is_me,omitempty"`
	Unread      int       `json:"unread,omitempty"`
	Highlights  int       `json:"mentions,omitempty"`
	BotUser     id.UserID `json:"bridge_bot,omitempty"`

	lastTS int64
	dirty  bool
}

type RoomCache struct {
	app     *App
	mu      sync.RWMutex
	rooms   map[id.RoomID]*RoomInfo
	invites map[id.RoomID]bool
	loading map[id.RoomID]bool // rooms joined while running that are being loaded
	loadSem chan struct{}
	act     map[id.RoomID]*activity
}

func NewRoomCache(app *App) *RoomCache {
	act, err := app.store.LoadActivity(context.Background())
	if err != nil {
		app.log.Warn().Err(err).Msg("failed to load room activity")
		act = map[id.RoomID]*activity{}
	}
	return &RoomCache{
		app: app, rooms: map[id.RoomID]*RoomInfo{}, invites: map[id.RoomID]bool{},
		loading: map[id.RoomID]bool{}, loadSem: make(chan struct{}, 4), act: act,
	}
}

func networkOf(userID id.UserID) (network string, isBot bool) {
	localpart, _, _ := userID.Parse()
	for _, n := range knownNetworks {
		if localpart == n+"bot" {
			return n, true
		}
		if strings.HasPrefix(localpart, n+"_") {
			return n, false
		}
	}
	return "", false
}

func (rc *RoomCache) LoadAll(ctx context.Context) {
	resp, err := rc.app.cli.JoinedRooms(ctx)
	if err != nil {
		rc.app.log.Error().Err(err).Msg("failed to list joined rooms")
		return
	}
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for _, roomID := range resp.JoinedRooms {
		wg.Add(1)
		sem <- struct{}{}
		go func(roomID id.RoomID) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := rc.load(ctx, roomID); err != nil {
				rc.app.log.Warn().Err(err).Stringer("room_id", roomID).Msg("failed to load room")
			}
		}(roomID)
	}
	wg.Wait()
}

func (rc *RoomCache) load(ctx context.Context, roomID id.RoomID) (*RoomInfo, error) {
	state, err := rc.app.cli.State(ctx, roomID)
	if err != nil {
		return nil, err
	}
	me := rc.app.cli.UserID
	info := &RoomInfo{ID: roomID}
	get := func(t event.Type) *event.Event {
		if m, ok := state[t]; ok {
			return m[""]
		}
		return nil
	}
	parse := func(evt *event.Event) {
		if evt != nil {
			_ = evt.Content.ParseRaw(evt.Type)
		}
	}
	if evt := get(event.StateRoomName); evt != nil {
		parse(evt)
		if c, ok := evt.Content.Parsed.(*event.RoomNameEventContent); ok {
			info.Name = c.Name
		}
	}
	if evt := get(event.StateTopic); evt != nil {
		parse(evt)
		if c, ok := evt.Content.Parsed.(*event.TopicEventContent); ok {
			info.Topic = c.Topic
		}
	}
	if evt := get(event.StateCanonicalAlias); evt != nil {
		parse(evt)
		if c, ok := evt.Content.Parsed.(*event.CanonicalAliasEventContent); ok {
			info.Alias = c.Alias.String()
		}
	}
	info.Encrypted = get(event.StateEncryption) != nil
	if evt := get(event.StateCreate); evt != nil {
		parse(evt)
		if c, ok := evt.Content.Parsed.(*event.CreateEventContent); ok && c.Type == event.RoomTypeSpace {
			info.Kind = "space"
		}
	}
	for _, bt := range []event.Type{event.StateBridge, event.StateHalfShotBridge} {
		for _, evt := range state[bt] {
			parse(evt)
			if c, ok := evt.Content.Parsed.(*event.BridgeEventContent); ok && c.Protocol.ID != "" {
				info.Network = normalizeNetwork(c.Protocol.ID)
			}
		}
	}
	var others []id.UserID
	for stateKey, evt := range state[event.StateMember] {
		parse(evt)
		c, ok := evt.Content.Parsed.(*event.MemberEventContent)
		if !ok || (c.Membership != event.MembershipJoin && c.Membership != event.MembershipInvite) {
			continue
		}
		info.Members++
		uid := id.UserID(stateKey)
		if uid != me {
			others = append(others, uid)
		}
	}
	var bot id.UserID
	ghosts := 0
	for _, uid := range others {
		n, isBot := networkOf(uid)
		if n != "" && info.Network == "" {
			info.Network = n
		}
		if isBot {
			bot = uid
		} else if n != "" {
			ghosts++
		}
	}
	if info.Kind == "" {
		switch {
		case len(others) == 1 && bot != "":
			info.Kind = "bridge_bot"
			info.BotUser = bot
		case len(others) == 1 || (len(others) == 2 && bot != "" && ghosts == 1):
			info.Kind = "dm"
		default:
			info.Kind = "group"
		}
	}
	if info.Name == "" {
		var names []string
		for _, uid := range others {
			if uid == bot && info.Kind != "bridge_bot" {
				continue
			}
			if evt := state[event.StateMember][uid.String()]; evt != nil {
				if c, ok := evt.Content.Parsed.(*event.MemberEventContent); ok && c.Displayname != "" {
					names = append(names, c.Displayname)
					continue
				}
			}
			names = append(names, uid.String())
		}
		sort.Strings(names)
		if len(names) > 4 {
			names = append(names[:4], fmt.Sprintf("and %d others", len(names)-4))
		}
		info.Name = strings.Join(names, ", ")
		if info.Name == "" {
			info.Name = roomID.String()
		}
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()
	if old, ok := rc.rooms[roomID]; ok {
		info.Tags = old.Tags
	}
	rc.applyActivity(info)
	rc.rooms[roomID] = info
	delete(rc.invites, roomID)
	return info, nil
}

func normalizeNetwork(protocol string) string {
	p := strings.ToLower(protocol)
	switch {
	case strings.Contains(p, "instagram"):
		return "instagram"
	case strings.Contains(p, "facebook"), strings.Contains(p, "messenger"), p == "meta":
		return "messenger"
	case strings.Contains(p, "whatsapp"):
		return "whatsapp"
	case strings.Contains(p, "discord"):
		return "discord"
	case strings.Contains(p, "imessage"):
		return "imessage"
	}
	return p
}

// applyActivity copies persisted activity into info. Caller holds rc.mu.
func (rc *RoomCache) applyActivity(info *RoomInfo) {
	if a, ok := rc.act[info.ID]; ok {
		info.lastTS = a.TS
		if a.TS > 0 {
			info.LastTime = fmtTS(a.TS)
		}
		info.LastPreview = a.Preview
		info.LastSender = a.Sender
		info.LastFromMe = a.Sender == "me"
		info.Unread = a.Unread
		info.Highlights = a.Highlights
	}
}

func (rc *RoomCache) Get(ctx context.Context, roomID id.RoomID) (*RoomInfo, error) {
	rc.mu.RLock()
	info, ok := rc.rooms[roomID]
	rc.mu.RUnlock()
	if ok && !info.dirty {
		return info, nil
	}
	return rc.load(ctx, roomID)
}

func (rc *RoomCache) MarkDirty(roomID id.RoomID) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if info, ok := rc.rooms[roomID]; ok {
		info.dirty = true
	}
}

func (rc *RoomCache) Forget(roomID id.RoomID) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	delete(rc.rooms, roomID)
	delete(rc.invites, roomID)
}

func (rc *RoomCache) AddInvite(roomID id.RoomID) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if _, joined := rc.rooms[roomID]; !joined {
		rc.invites[roomID] = true
	}
}

// NoteJoined records that the account is joined to a room, so it is no longer a pending invite.
// It returns true when the room is new to the cache and the caller should load it; the caller
// must then call FinishLoading. With loadNew false (during startup, when LoadAll covers every
// joined room) it only clears the invite.
func (rc *RoomCache) NoteJoined(roomID id.RoomID, loadNew bool) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	delete(rc.invites, roomID)
	if _, known := rc.rooms[roomID]; known || rc.loading[roomID] || !loadNew {
		return false
	}
	rc.loading[roomID] = true
	return true
}

// FinishLoading ends a load started by NoteJoined. A failed load is retried on the room's next sync.
func (rc *RoomCache) FinishLoading(roomID id.RoomID) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	delete(rc.loading, roomID)
}

func (rc *RoomCache) Invites() []id.RoomID {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	out := make([]id.RoomID, 0, len(rc.invites))
	for r := range rc.invites {
		out = append(out, r)
	}
	return out
}

func (rc *RoomCache) Count() int {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return len(rc.rooms)
}

func (rc *RoomCache) activityFor(roomID id.RoomID) *activity {
	a, ok := rc.act[roomID]
	if !ok {
		a = &activity{}
		rc.act[roomID] = a
	}
	return a
}

func (rc *RoomCache) SetUnread(ctx context.Context, roomID id.RoomID, unread, highlights int) {
	rc.mu.Lock()
	a := rc.activityFor(roomID)
	a.Unread, a.Highlights = unread, highlights
	if info, ok := rc.rooms[roomID]; ok {
		info.Unread, info.Highlights = unread, highlights
	}
	snapshot := *a
	rc.mu.Unlock()
	rc.app.store.SaveActivity(ctx, roomID, &snapshot)
}

func (rc *RoomCache) Touch(ctx context.Context, roomID id.RoomID, m *Message) {
	rc.mu.Lock()
	a := rc.activityFor(roomID)
	if m.TS <= a.TS {
		rc.mu.Unlock()
		return
	}
	a.TS = m.TS
	preview := m.Body
	if preview == "" && m.Media != nil {
		preview = "[" + m.Kind + "] " + m.Media.Filename
	}
	if len([]rune(preview)) > 140 {
		preview = string([]rune(preview)[:140]) + "…"
	}
	a.Preview = preview
	a.Sender = m.SenderName
	if m.IsMe {
		a.Sender = "me"
	}
	if info, ok := rc.rooms[roomID]; ok {
		rc.applyActivity(info)
	}
	snapshot := *a
	rc.mu.Unlock()
	rc.app.store.SaveActivity(ctx, roomID, &snapshot)
}

func (rc *RoomCache) SetTags(roomID id.RoomID, evt *event.Event) {
	_ = evt.Content.ParseRaw(evt.Type)
	c, ok := evt.Content.Parsed.(*event.TagEventContent)
	if !ok {
		return
	}
	var tags []string
	for tag := range c.Tags {
		tags = append(tags, strings.TrimPrefix(string(tag), "m."))
	}
	sort.Strings(tags)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if info, ok := rc.rooms[roomID]; ok {
		info.Tags = tags
	}
}

// List returns all rooms sorted by most recent activity.
func (rc *RoomCache) List() []*RoomInfo {
	rc.mu.RLock()
	out := make([]*RoomInfo, 0, len(rc.rooms))
	for _, r := range rc.rooms {
		out = append(out, r)
	}
	rc.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].lastTS > out[j].lastTS })
	return out
}

// Resolve accepts a room ID, an alias or a unique (case-insensitive) part of a room name.
func (app *App) resolveRoom(ctx context.Context, ref string) (*RoomInfo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("room is required")
	}
	if strings.HasPrefix(ref, "!") {
		return app.rooms.Get(ctx, id.RoomID(ref))
	}
	if strings.HasPrefix(ref, "#") {
		resp, err := app.cli.ResolveAlias(ctx, id.RoomAlias(ref))
		if err != nil {
			return nil, fmt.Errorf("resolve alias: %w", err)
		}
		return app.rooms.Get(ctx, resp.RoomID)
	}
	needle := strings.ToLower(ref)
	var exact, partial []*RoomInfo
	for _, r := range app.rooms.List() {
		name := strings.ToLower(r.Name)
		if name == needle {
			exact = append(exact, r)
		} else if strings.Contains(name, needle) {
			partial = append(partial, r)
		}
	}
	matches := exact
	if len(matches) == 0 {
		matches = partial
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no room matches %q; use list_rooms to find it", ref)
	case 1:
		return app.rooms.Get(ctx, matches[0].ID)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%q matches %d rooms, pass a room_id instead:", ref, len(matches))
	for i, r := range matches {
		if i == 10 {
			b.WriteString("\n…")
			break
		}
		fmt.Fprintf(&b, "\n- %s (%s, %s) %s", r.Name, r.Kind, r.Network, r.ID)
	}
	return nil, fmt.Errorf("%s", b.String())
}
