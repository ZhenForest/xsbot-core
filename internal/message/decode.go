// Package message strictly normalizes the actual OneBot wire structure.
package message

import (
	"fmt"
	"strconv"
	"time"
	"github.com/ZhenForest/xsbot-core/internal/strictjson"
	"github.com/ZhenForest/xsbot-core/sdk"
)

type Failure struct {
	Reason   string
	Location string
	Text     []string
}

func (f *Failure) Error() string { return f.Location + ": " + f.Reason }
func Int(v *strictjson.Value, allowString bool) (int64, error) {
	if v == nil || (v.Kind != strictjson.Number && !(allowString && v.Kind == strictjson.String)) {
		return 0, fmt.Errorf("expected exact integer")
	}
	return strconv.ParseInt(v.Text, 10, 64)
}
func required(f map[string]*strictjson.Value, k string) (string, error) { return f[k].RequireString() }
func optionalString(v *strictjson.Value) (sdk.Optional[string], error) {
	if v == nil {
		return sdk.Optional[string]{}, nil
	}
	if v.Kind == strictjson.Null {
		return sdk.Optional[string]{}, fmt.Errorf("null not allowed")
	}
	s, e := v.RequireString()
	return sdk.Optional[string]{State: sdk.Present, Value: s}, e
}
func Decode(data []byte, received time.Time) (sdk.Message, error) {
	v, e := strictjson.Parse(data)
	if e != nil {
		return sdk.Message{}, &Failure{Reason: e.Error(), Location: "$"}
	}
	return DecodeValue(v, received, false)
}
func DecodeValue(v *strictjson.Value, received time.Time, query bool) (result sdk.Message, err error) {
	fail := func(location string, e error) (sdk.Message, error) {
		return sdk.Message{}, &Failure{Reason: e.Error(), Location: location, Text: ExtractText(v)}
	}
	keys := []string{"time", "message_type", "message_id", "real_id", "message_seq", "real_seq", "user_id", "group_id", "sender", "message", "raw_message", "font", "emoji_likes_list"}
	keys = append(keys, "self_id", "post_type", "sub_type", "message_format", "group_name", "target_id", "message_sent_type", "temp_source")
	f, e := v.Fields(keys...)
	if e != nil {
		return fail("$", e)
	}
	if !query || f["post_type"] != nil || f["message_format"] != nil {
		post, e := required(f, "post_type")
		if e != nil || post != "message" && post != "message_sent" {
			return fail("$.post_type", fmt.Errorf("unsupported event"))
		}
		format, e := required(f, "message_format")
		if e != nil || format != "array" {
			return fail("$.message_format", fmt.Errorf("array format required"))
		}
	}
	typ, e := required(f, "message_type")
	if e != nil || typ != "private" && typ != "group" {
		return fail("$.message_type", fmt.Errorf("unsupported scene"))
	}
	if !query || f["sub_type"] != nil {
		sub, e := required(f, "sub_type")
		if e != nil || typ == "private" && sub != "friend" || typ == "group" && sub != "normal" {
			return fail("$.sub_type", fmt.Errorf("unsupported subtype"))
		}
	}
	id, e := Int(f["message_id"], false)
	if e != nil || id == 0 {
		return fail("$.message_id", fmt.Errorf("invalid message ID"))
	}
	user, e := Int(f["user_id"], true)
	if e != nil || user <= 0 {
		return fail("$.user_id", fmt.Errorf("invalid user ID"))
	}
	result.Ref = sdk.MessageRef{ID: id, Target: sdk.Target{Scene: sdk.Scene(typ), ID: user}}
	result.ReceivedAt = received.UTC()
	if typ == "group" {
		group, e := Int(f["group_id"], true)
		if e != nil || group <= 0 {
			return fail("$.group_id", fmt.Errorf("invalid group ID"))
		}
		result.Ref.Target.ID = group
	}
	if !query || f["self_id"] != nil {
		bot, e := Int(f["self_id"], false)
		if e != nil || bot <= 0 {
			return fail("$.self_id", fmt.Errorf("invalid bot ID"))
		}
		result.BotID = sdk.Optional[int64]{State: sdk.Present, Value: bot}
		if typ == "private" && user == bot && (!query || f["target_id"] != nil) {
			target, e := Int(f["target_id"], false)
			if e != nil || target <= 0 {
				return fail("$.target_id", fmt.Errorf("missing self-message peer"))
			}
			result.Ref.Target.ID = target
		}
	}
	sec, e := Int(f["time"], false)
	if e != nil || sec <= 0 {
		return fail("$.time", fmt.Errorf("invalid Unix seconds"))
	}
	tm := time.Unix(sec, 0).UTC()
	result.MessageTime = sdk.Optional[time.Time]{State: sdk.Present, Value: tm}
	// OneBot supplies one timestamp, not independently attested event and message times.
	if f["real_seq"] != nil {
		seq, e := Int(f["real_seq"], true)
		if e != nil {
			return fail("$.real_seq", e)
		}
		result.Sequence = sdk.Optional[int64]{State: sdk.Present, Value: seq}
	}
	sender, e := f["sender"].Fields("user_id", "nickname", "card", "role", "sex", "age", "area", "level", "title")
	if e != nil {
		return fail("$.sender", e)
	}
	senderID, e := Int(sender["user_id"], true)
	if e != nil || senderID != user {
		return fail("$.sender.user_id", fmt.Errorf("invalid sender identity"))
	}
	result.Sender.ID = user
	result.Sender.Nickname, e = optionalString(sender["nickname"])
	if e != nil {
		return fail("$.sender.nickname", e)
	}
	result.Sender.Card, e = optionalString(sender["card"])
	if e != nil {
		return fail("$.sender.card", e)
	}
	result.Sender.Role, e = optionalString(sender["role"])
	if e != nil {
		return fail("$.sender.role", e)
	}
	result.Metadata = map[string]sdk.Scalar{}
	for _, k := range []string{"raw_message", "group_name", "message_sent_type"} {
		if f[k] != nil {
			if _, e := f[k].RequireString(); e != nil {
				return fail("$."+k, e)
			}
		}
	}
	for _, k := range []string{"message_seq", "real_id", "font", "target_id", "temp_source"} {
		if f[k] != nil {
			n, e := Int(f[k], false)
			if e != nil {
				return fail("$."+k, e)
			}
			result.Metadata[k] = sdk.Scalar{State: sdk.Present, Integer: &n}
		}
	}
	for _, k := range []string{"sex", "area", "level", "title", "age"} {
		if x := sender[k]; x != nil {
			var n sdk.Scalar
			n.State = sdk.Present
			if k == "age" {
				v, e := Int(x, false)
				if e != nil {
					return fail("$.sender."+k, e)
				}
				n.Integer = &v
			} else {
				s, e := x.RequireString()
				if e != nil {
					return fail("$.sender."+k, e)
				}
				n.String = &s
			}
			result.Metadata["sender."+k] = n
		}
	}
	if x := f["group_name"]; x != nil {
		s := x.Text
		result.Metadata["group_name"] = sdk.Scalar{State: sdk.Present, String: &s}
	}
	if x := f["message_sent_type"]; x != nil {
		s := x.Text
		result.Metadata["message_sent_type"] = sdk.Scalar{State: sdk.Present, String: &s}
	}
	if likes := f["emoji_likes_list"]; likes != nil {
		if likes.Kind != strictjson.Array {
			return fail("$.emoji_likes_list", fmt.Errorf("expected reaction array"))
		}
		list := []sdk.Scalar{}
		for _, like := range likes.Array {
			f, e := like.Fields("emoji_id", "emoji_type", "likes_cnt")
			if e != nil {
				return fail("$.emoji_likes_list", e)
			}
			metadata := map[string]sdk.Scalar{}
			for key, value := range f {
				n, e := Int(value, key == "emoji_id")
				if e != nil {
					return fail("$.emoji_likes_list."+key, e)
				}
				metadata[key] = sdk.Scalar{State: sdk.Present, Integer: &n}
			}
			list = append(list, sdk.Scalar{State: sdk.Present, Object: metadata})
		}
		result.Metadata["emoji_likes_list"] = sdk.Scalar{State: sdk.Present, List: list}
	}
	msg := f["message"]
	if msg == nil || msg.Kind != strictjson.Array {
		return fail("$.message", fmt.Errorf("expected segment array"))
	}
	for i, value := range msg.Array {
		location := fmt.Sprintf("$.message[%d]", i)
		segment, e := decodeSegment(value, result.Ref.Target, location)
		if e != nil {
			// Determine the reply boundary without ignoring duplicate outer segment fields.
			if replyBoundary(value) {
				segment = sdk.Segment{Kind: sdk.Reply, Unknown: &sdk.Unknown{Reason: e.Error(), Location: location}}
			} else {
				return fail(location, e)
			}
		}
		if segment.Kind == sdk.Mention && result.BotID.State == sdk.Present && segment.UserID == result.BotID.Value {
			result.WasAtBot = true
		}
		result.Segments = append(result.Segments, segment)
	}
	return result, nil
}
func replyBoundary(v *strictjson.Value) bool {
	if v == nil || v.Kind != strictjson.Object {
		return false
	}
	count := 0
	reply := false
	for _, p := range v.Object {
		if p.Key == "type" {
			count++
			reply = p.Value.Kind == strictjson.String && p.Value.Text == "reply"
		}
	}
	return count == 1 && reply
}
func decodeSegment(v *strictjson.Value, target sdk.Target, location string) (sdk.Segment, error) {
	var s sdk.Segment
	f, e := v.Fields("type", "data")
	if e != nil {
		return s, e
	}
	kind, e := required(f, "type")
	if e != nil {
		return s, e
	}
	s.Kind = sdk.SegmentKind(kind)
	switch kind {
	case "text":
		d, e := f["data"].Fields("text")
		if e != nil {
			return s, e
		}
		s.Text, e = required(d, "text")
		return s, e
	case "at":
		d, e := f["data"].Fields("qq", "name")
		if e != nil {
			return s, e
		}
		q, e := required(d, "qq")
		if e != nil {
			return s, e
		}
		if q == "all" {
			s.Kind = sdk.MentionAll
		} else {
			s.Kind = sdk.Mention
			s.UserID, e = strconv.ParseInt(q, 10, 64)
			if e != nil || s.UserID <= 0 {
				return s, fmt.Errorf("invalid At QQ")
			}
		}
		if d["name"] != nil {
			_, e = d["name"].RequireString()
		}
		return s, e
	case "reply":
		d, e := f["data"].Fields("id")
		if e != nil {
			return s, e
		}
		id, e := Int(d["id"], true)
		if e != nil || id == 0 {
			return s, fmt.Errorf("invalid reply ID")
		}
		s.Ref = &sdk.MessageRef{Target: target, ID: id}
		return s, nil
	case "json", "xml", "markdown":
		key := "data"
		if kind == "markdown" {
			key = "content"
		}
		d, e := f["data"].Fields(key)
		if e != nil {
			return s, e
		}
		s.Payload, e = required(d, key)
		return s, e
	case "image", "record", "video", "file":
		allowed := []string{"file", "url", "path", "file_id", "file_unique", "file_size", "name", "summary", "sub_type", "thumb"}
		if kind == "image" {
			allowed = append(allowed, "emoji_id", "emoji_package_id", "key")
		}
		d, e := f["data"].Fields(allowed...)
		if e != nil {
			return s, e
		}
		if _, e = required(d, "file"); e != nil {
			return s, e
		}
		s.Metadata = map[string]sdk.Scalar{}
		for k, x := range d {
			scalar := sdk.Scalar{State: sdk.Present}
			if k == "sub_type" || k == "emoji_package_id" {
				n, e := Int(x, false)
				if e != nil {
					return s, fmt.Errorf("%s: %w", k, e)
				}
				scalar.Integer = &n
			} else if k == "file_size" {
				n, e := Int(x, true)
				if e != nil || n < 0 {
					return s, fmt.Errorf("invalid file_size")
				}
				scalar.Integer = &n
			} else {
				str, e := x.RequireString()
				if e != nil {
					return s, fmt.Errorf("%s: %w", k, e)
				}
				scalar.String = &str
			}
			s.Metadata[k] = scalar
		}
		if d["emoji_id"] != nil || d["emoji_package_id"] != nil || d["key"] != nil {
			if kind != "image" || d["emoji_id"] == nil || d["emoji_package_id"] == nil || d["key"] == nil {
				return s, fmt.Errorf("incomplete market face")
			}
			s.Kind = sdk.MarketFace
		}
		return s, nil
	case "face":
		d, e := f["data"].Fields("id", "raw", "resultId", "chainCount")
		if e != nil {
			return s, e
		}
		if _, e = required(d, "id"); e != nil {
			return s, e
		}
		s.Metadata = map[string]sdk.Scalar{}
		for k, x := range d {
			if k == "raw" {
				raw, e := x.Fields("faceIndex", "faceType", "faceText", "stickerId", "stickerType", "packId", "sourceType", "resultId", "chainCount", "pokeType", "surpriseId", "randomType", "imageType", "spokeSummary", "doubleHit", "vaspokeId", "vaspokeName", "vaspokeMinver", "pokeStrength", "msgType", "faceBubbleCount", "oldVersionStr", "pokeFlag")
				if e != nil {
					return s, e
				}
				for key, value := range raw {
					// QQ's native FaceElement includes null optional fields. Preserve
					// null distinctly; extra fields observed only as null stay null-only.
					if value.Kind == strictjson.Null && key != "faceIndex" && key != "faceType" {
						s.Metadata["raw."+key] = sdk.Scalar{State: sdk.Null}
						continue
					}
					switch key {
					case "imageType", "spokeSummary", "doubleHit", "vaspokeId", "vaspokeName", "vaspokeMinver", "pokeStrength", "msgType", "faceBubbleCount", "oldVersionStr", "pokeFlag":
						return s, fmt.Errorf("unsupported non-null raw face field %s", key)
					}
					strings := map[string]bool{"faceText": true, "stickerId": true, "packId": true, "resultId": true, "surpriseId": true}
					if strings[key] && value.Kind != strictjson.String || !strings[key] && value.Kind != strictjson.Number {
						return s, fmt.Errorf("invalid raw face field %s", key)
					}
					scalar, e := faceScalar(value)
					if e != nil {
						return s, e
					}
					s.Metadata["raw."+key] = scalar
				}
				continue
			}
			if x.Kind == strictjson.Null && (k == "resultId" || k == "chainCount") {
				s.Metadata[k] = sdk.Scalar{State: sdk.Null}
				continue
			}
			if k == "chainCount" && x.Kind != strictjson.Number || k != "chainCount" && x.Kind != strictjson.String {
				return s, fmt.Errorf("invalid face field %s", k)
			}
			scalar, e := faceScalar(x)
			if e != nil {
				return s, e
			}
			s.Metadata[k] = scalar
		}
		return s, nil
	case "forward":
		d, e := f["data"].Fields("id", "content")
		if e != nil {
			return s, e
		}
		id, e := required(d, "id")
		if e != nil {
			return s, e
		}
		s.Metadata = map[string]sdk.Scalar{"id": {State: sdk.Present, String: &id}}
		if content := d["content"]; content != nil {
			if content.Kind != strictjson.Array {
				return s, fmt.Errorf("invalid forward content")
			}
			for _, child := range content.Array {
				msg, e := DecodeValue(child, time.Now(), true)
				if e != nil {
					return s, e
				}
				s.Messages = append(s.Messages, msg)
			}
		}
		return s, nil
	default:
		return s, fmt.Errorf("unsupported segment %q", kind)
	}
}
func faceScalar(v *strictjson.Value) (sdk.Scalar, error) {
	s := sdk.Scalar{State: sdk.Present}
	if v.Kind == strictjson.String {
		x := v.Text
		s.String = &x
		return s, nil
	}
	if v.Kind == strictjson.Number {
		n, e := Int(v, false)
		s.Integer = &n
		return s, e
	}
	return s, fmt.Errorf("invalid face scalar")
}

// ExtractText only visits actual direct segment-array positions and validates each whole text segment.
func ExtractText(v *strictjson.Value) []string {
	var out []string
	if v == nil || v.Kind != strictjson.Object {
		return out
	}
	count := 0
	var m *strictjson.Value
	for _, p := range v.Object {
		if p.Key == "message" {
			count++
			m = p.Value
		}
	}
	if count != 1 || m.Kind != strictjson.Array {
		return out
	}
	for _, segment := range m.Array {
		f, e := segment.Fields("type", "data")
		if e != nil {
			continue
		}
		typ, e := required(f, "type")
		if e != nil || typ != "text" {
			continue
		}
		d, e := f["data"].Fields("text")
		if e != nil {
			continue
		}
		t, e := required(d, "text")
		if e == nil {
			out = append(out, t)
		}
	}
	return out
}
