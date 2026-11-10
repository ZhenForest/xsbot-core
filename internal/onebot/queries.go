package onebot

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"github.com/ZhenForest/xsbot-core/internal/media"
	"github.com/ZhenForest/xsbot-core/internal/message"
	"github.com/ZhenForest/xsbot-core/internal/strictjson"
	"github.com/ZhenForest/xsbot-core/sdk"
)

func protocol(reason string) error { return &sdk.Error{Kind: sdk.ProtocolFailure, Reason: reason} }

// The scalar rules are per-field declarations; unlisted fields and complex values fail closed.
func scalars(v *strictjson.Value, spec map[string]string) (map[string]sdk.Scalar, error) {
	keys := make([]string, 0, len(spec))
	for k := range spec {
		keys = append(keys, k)
	}
	fields, e := v.Fields(keys...)
	if e != nil {
		return nil, protocol(e.Error())
	}
	out := map[string]sdk.Scalar{}
	for k, x := range fields {
		kind := spec[k]
		s := sdk.Scalar{State: sdk.Present}
		if x.Kind == strictjson.Null && strings.HasSuffix(kind, "?") {
			s.State = sdk.Null
			out[k] = s
			continue
		}
		switch strings.TrimSuffix(kind, "?") {
		case "s":
			value, e := x.RequireString()
			if e != nil {
				return nil, protocol("invalid " + k)
			}
			s.String = &value
		case "i", "q":
			n, e := integer(x, kind == "q")
			if e != nil {
				return nil, protocol("invalid " + k)
			}
			s.Integer = &n
		case "b":
			if x.Kind != strictjson.Boolean {
				return nil, protocol("invalid " + k)
			}
			b := x.Bool
			s.Boolean = &b
		}
		out[k] = s
	}
	return out, nil
}
func str(m map[string]sdk.Scalar, k string) sdk.Optional[string] {
	v, ok := m[k]
	if !ok {
		return sdk.Optional[string]{}
	}
	if v.State == sdk.Null {
		return sdk.Optional[string]{State: sdk.Null}
	}
	if v.String == nil {
		return sdk.Optional[string]{}
	}
	return sdk.Optional[string]{State: sdk.Present, Value: *v.String}
}
func id(m map[string]sdk.Scalar, k string) (int64, error) {
	v, ok := m[k]
	if !ok || v.Integer == nil || *v.Integer <= 0 {
		return 0, protocol("missing or invalid " + k)
	}
	return *v.Integer, nil
}
func decodeUser(v *strictjson.Value, profile bool) (sdk.User, error) {
	if profile {
		decoded, e := decodeSchema(v, querySchemas["user"])
		if e != nil {
			return sdk.User{}, protocol(e.Error())
		}
		user, e := id(decoded.Object, "user_id")
		if e != nil {
			return sdk.User{}, e
		}
		return sdk.User{ID: user, Nickname: str(decoded.Object, "nickname"), Remark: str(decoded.Object, "remark"), Metadata: decoded.Object}, nil
	}
	spec := map[string]string{"user_id": "q", "nickname": "s", "remark": "s", "birthday_year": "i", "birthday_month": "i", "birthday_day": "i", "age": "i", "phone_num": "s", "email": "s", "category_id": "i", "sex": "s", "level": "i"}
	metadata, e := scalars(v, spec)
	if e != nil {
		return sdk.User{}, e
	}
	userID, e := id(metadata, "user_id")
	if e != nil {
		return sdk.User{}, e
	}
	return sdk.User{ID: userID, Nickname: str(metadata, "nickname"), Remark: str(metadata, "remark"), Metadata: metadata}, nil
}
func (c *Client) LoginInfo(ctx context.Context) (sdk.User, error) {
	v, e := c.call(ctx, "get_login_info", struct{}{})
	if e != nil {
		return sdk.User{}, e
	}
	return decodeUser(v, false)
}
func (c *Client) UserInfo(ctx context.Context, user int64) (sdk.User, error) {
	if user <= 0 {
		return sdk.User{}, &sdk.Error{Kind: sdk.Invalid, Reason: "invalid user ID"}
	}
	v, e := c.call(ctx, "get_stranger_info", map[string]any{"user_id": strconv.FormatInt(user, 10), "no_cache": false})
	if e != nil {
		return sdk.User{}, e
	}
	return decodeUser(v, true)
}
func (c *Client) Friends(ctx context.Context) ([]sdk.User, error) {
	v, e := c.call(ctx, "get_friend_list", map[string]bool{"no_cache": false})
	if e != nil {
		return nil, e
	}
	if v.Kind != strictjson.Array {
		return nil, protocol("expected friend array")
	}
	out := make([]sdk.User, 0, len(v.Array))
	for _, x := range v.Array {
		u, e := decodeUser(x, false)
		if e != nil {
			return nil, e
		}
		out = append(out, u)
	}
	return out, nil
}
func (c *Client) FriendInfo(ctx context.Context, user int64) (sdk.User, error) {
	if user <= 0 {
		return sdk.User{}, &sdk.Error{Kind: sdk.Invalid, Reason: "invalid user ID"}
	}
	list, e := c.Friends(ctx)
	if e != nil {
		return sdk.User{}, e
	}
	for _, u := range list {
		if u.ID == user {
			return u, nil
		}
	}
	return sdk.User{}, protocol("user is not in returned friend list")
}
func decodeGroup(v *strictjson.Value) (sdk.GroupInfo, error) {
	decoded, e := decodeSchema(v, querySchemas["group"])
	m := decoded.Object
	if e != nil {
		return sdk.GroupInfo{}, e
	}
	group, e := id(m, "group_id")
	name := str(m, "group_name")
	if e != nil || name.State != sdk.Present {
		return sdk.GroupInfo{}, protocol("invalid group identity")
	}
	return sdk.GroupInfo{ID: group, Name: name.Value, Metadata: m}, nil
}
func (c *Client) GroupInfo(ctx context.Context, group int64) (sdk.GroupInfo, error) {
	if group <= 0 {
		return sdk.GroupInfo{}, &sdk.Error{Kind: sdk.Invalid, Reason: "invalid group ID"}
	}
	v, e := c.call(ctx, "get_group_info", map[string]string{"group_id": fmt.Sprint(group)})
	if e != nil {
		return sdk.GroupInfo{}, e
	}
	return decodeGroup(v)
}
func (c *Client) Groups(ctx context.Context) ([]sdk.GroupInfo, error) {
	v, e := c.call(ctx, "get_group_list", struct{}{})
	if e != nil {
		return nil, e
	}
	if v.Kind != strictjson.Array {
		return nil, protocol("expected group array")
	}
	out := make([]sdk.GroupInfo, 0, len(v.Array))
	for _, x := range v.Array {
		g, e := decodeGroup(x)
		if e != nil {
			return nil, e
		}
		out = append(out, g)
	}
	return out, nil
}
func decodeMember(v *strictjson.Value) (sdk.Member, error) {
	m, e := scalars(v, map[string]string{"group_id": "q", "user_id": "q", "nickname": "s", "card": "s", "sex": "s", "age": "i", "area": "s", "level": "s", "qq_level": "i", "join_time": "i", "last_sent_time": "i", "title_expire_time": "i", "unfriendly": "b", "card_changeable": "b", "is_robot": "b", "shut_up_timestamp": "i", "role": "s", "title": "s"})
	if e != nil {
		return sdk.Member{}, e
	}
	group, e := id(m, "group_id")
	if e != nil {
		return sdk.Member{}, e
	}
	user, e := id(m, "user_id")
	if e != nil {
		return sdk.Member{}, e
	}
	return sdk.Member{GroupID: group, User: sdk.User{ID: user, Nickname: str(m, "nickname"), Metadata: m}, Role: str(m, "role"), Metadata: m}, nil
}
func (c *Client) MemberInfo(ctx context.Context, group, user int64) (sdk.Member, error) {
	if group <= 0 || user <= 0 {
		return sdk.Member{}, &sdk.Error{Kind: sdk.Invalid, Reason: "invalid member target"}
	}
	v, e := c.call(ctx, "get_group_member_info", map[string]any{"group_id": fmt.Sprint(group), "user_id": fmt.Sprint(user), "no_cache": false})
	if e != nil {
		return sdk.Member{}, e
	}
	return decodeMember(v)
}
func (c *Client) Members(ctx context.Context, group int64) ([]sdk.Member, error) {
	if group <= 0 {
		return nil, &sdk.Error{Kind: sdk.Invalid, Reason: "invalid group ID"}
	}
	v, e := c.call(ctx, "get_group_member_list", map[string]string{"group_id": fmt.Sprint(group)})
	if e != nil {
		return nil, e
	}
	if v.Kind != strictjson.Array {
		return nil, protocol("expected member array")
	}
	out := make([]sdk.Member, 0, len(v.Array))
	for _, x := range v.Array {
		m, e := decodeMember(x)
		if e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, nil
}
func (c *Client) GetMessage(ctx context.Context, ref sdk.MessageRef) (sdk.Message, error) {
	if e := ref.Target.Validate(); e != nil {
		return sdk.Message{}, e
	}
	if ref.ID == 0 {
		return sdk.Message{}, &sdk.Error{Kind: sdk.Invalid, Reason: "missing message ID"}
	}
	v, e := c.call(ctx, "get_msg", map[string]int64{"message_id": ref.ID})
	if e != nil {
		return sdk.Message{}, e
	}
	m, e := message.DecodeValue(v, time.Now(), true)
	if e != nil {
		return sdk.Message{}, protocol("unrecognized queried message: " + e.Error())
	}
	if m.Ref.Target != ref.Target || m.Ref.ID != ref.ID {
		return sdk.Message{}, protocol("queried message target mismatch")
	}
	return m, nil
}
func (c *Client) OpenResource(ctx context.Context, segment sdk.Segment) (io.ReadCloser, error) {
	for _, k := range []string{"url", "path"} {
		value := segment.Metadata[k]
		if value.String == nil || *value.String == "" {
			continue
		}
		s := *value.String
		if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
			return media.HTTPOpen(s)(ctx)
		}
		if k == "path" {
			return os.Open(s)
		}
	}
	value := segment.Metadata["file_id"]
	if value.String == nil {
		value = segment.Metadata["file"]
	}
	if value.String == nil {
		return nil, &sdk.Error{Kind: sdk.Unsupported, Reason: "no usable resource locator attached"}
	}
	v, e := c.call(ctx, "get_file", map[string]string{"file": *value.String})
	if e != nil {
		return nil, e
	}
	m, e := scalars(v, map[string]string{"file": "s", "url": "s", "file_size": "s", "file_name": "s", "base64": "s"})
	if e != nil {
		return nil, e
	}
	if path := str(m, "file"); path.State == sdk.Present && path.Value != "" {
		return os.Open(path.Value)
	}
	if address := str(m, "url"); address.State == sdk.Present {
		return media.HTTPOpen(address.Value)(ctx)
	}
	return nil, protocol("resource path absent")
}
