package main

import "encoding/json"

// Wire protocol between the `sidebar-go serve` daemon and `sidebar-go display`
// clients over the UDS stream at sidebarstate.DaemonSocketPath(). One JSON
// value per message, streamed with json.Encoder/json.Decoder — the decoder
// frames values off the stream, so no length prefix is needed. See
// plans/.../architecture-daemon-thin-clients.md for the full design.

// protoVersion is bumped on ANY wire-schema change. The hello/welcome
// handshake compares it: a mismatch tells the client to re-exec onto the new
// binary (or fall back to standalone). A pure-logic change that doesn't touch
// the wire keeps this constant so running clients are undisturbed.
const protoVersion = 3

// Message type tags (Envelope.T).
const (
	msgHello    = "hello"    // client → daemon: first frame after connect
	msgIntent   = "intent"   // client → daemon: user action that mutates state
	msgBye      = "bye"      // client → daemon: graceful close (optional)
	msgWelcome  = "welcome"  // daemon → client: handshake ack
	msgSnapshot = "snapshot" // daemon → client: full canonical state
	msgReexec   = "reexec"   // daemon → client: about to re-exec for upgrade
)

// Intent actions (intentMsg.Action).
const (
	actionCursor       = "cursor"        // move global selection to PaneID
	actionScroll       = "scroll"        // set shared view offset / pinned
	actionClearDone    = "clear_done"    // user visited PaneID; drop its done badge
	actionReload       = "reload"        // force an immediate loadTree
	actionToggleHidden = "toggle_hidden" // flip Session in the hidden-workspaces set
)

// Envelope is the type-tagged frame. Decode the envelope, switch on T, then
// unmarshal D into the concrete payload type.
type Envelope struct {
	T string          `json:"t"`
	D json.RawMessage `json:"d,omitempty"`
}

// helloMsg identifies a connecting client and its protocol version.
type helloMsg struct {
	Proto    int    `json:"proto"`
	PID      int    `json:"pid"`
	PaneID   string `json:"pane_id,omitempty"`   // diagnostic + future per-client views
	WindowID string `json:"window_id,omitempty"` // diagnostic
}

// welcomeMsg acks the handshake. Ok=false means proto mismatch — the client
// re-execs itself (picks up a newer binary) or falls back to standalone.
type welcomeMsg struct {
	Proto int  `json:"proto"`
	Ok    bool `json:"ok"`
}

// intentMsg is a user action the daemon applies to canonical state. Fields
// beyond Action are action-specific and may be zero.
type intentMsg struct {
	Action  string `json:"action"`
	PaneID  string `json:"pane_id,omitempty"`
	Session string `json:"session,omitempty"` // actionToggleHidden: workspace to flip
	YOffset int    `json:"y_offset,omitempty"`
	Pinned  bool   `json:"pinned,omitempty"`
}

// StateSnapshot is the full canonical state pushed to every client on connect
// and on every (debounced) change. It embeds the existing sharedState shape
// verbatim so the client reuses its established apply path, plus usage for the
// client-rendered footer.
type StateSnapshot struct {
	sharedState
	Usage UsagePeriods `json:"usage"`
}

// writeMsg encodes one envelope with payload to w. The caller serializes
// writes per-connection (the daemon hub does this via a per-conn channel).
func writeMsg(enc *json.Encoder, t string, payload any) error {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = b
	}
	return enc.Encode(Envelope{T: t, D: raw})
}

// readEnvelope decodes one framed envelope off the stream. Returns io.EOF when
// the peer closes — callers treat that as a normal disconnect.
func readEnvelope(dec *json.Decoder) (Envelope, error) {
	var e Envelope
	err := dec.Decode(&e)
	return e, err
}

// decodePayload unmarshals an envelope's D into v.
func decodePayload(e Envelope, v any) error {
	if len(e.D) == 0 {
		return nil
	}
	return json.Unmarshal(e.D, v)
}
