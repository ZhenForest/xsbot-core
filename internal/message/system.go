package message

import (
	"fmt"
	"github.com/ZhenForest/xsbot-core/internal/strictjson"
)

func System(v *strictjson.Value) (string, bool, error) {
	if v == nil || v.Kind != strictjson.Object {
		return "", false, nil
	}
	post := v.Get("post_type")
	if post == nil || post.Kind != strictjson.String || post.Text != "meta_event" {
		return "", false, nil
	}
	common := []string{"time", "self_id", "post_type", "meta_event_type", "sub_type", "interval", "status"}
	f, e := v.Fields(common...)
	if e != nil {
		return "", true, e
	}
	kind, e := required(f, "meta_event_type")
	if e != nil {
		return "", true, e
	}
	for _, k := range []string{"time", "self_id"} {
		n, e := Int(f[k], false)
		if e != nil || n <= 0 {
			return "", true, fmt.Errorf("invalid system %s", k)
		}
	}
	switch kind {
	case "lifecycle":
		if _, e = v.Fields("time", "self_id", "post_type", "meta_event_type", "sub_type"); e != nil {
			return "", true, e
		}
		sub, e := required(f, "sub_type")
		if e != nil || sub != "connect" {
			return "", true, fmt.Errorf("unsupported lifecycle")
		}
	case "heartbeat":
		if _, e = v.Fields("time", "self_id", "post_type", "meta_event_type", "status", "interval"); e != nil {
			return "", true, e
		}
		interval, e := Int(f["interval"], false)
		if e != nil || interval <= 0 {
			return "", true, fmt.Errorf("invalid heartbeat interval")
		}
		status, e := f["status"].Fields("online", "good", "stat")
		if e != nil {
			return "", true, fmt.Errorf("invalid heartbeat status: %w", e)
		}
		for _, k := range []string{"online", "good"} {
			if status[k] == nil || status[k].Kind != strictjson.Boolean {
				return "", true, fmt.Errorf("invalid heartbeat status")
			}
		}
		// NapCat's pinned heartbeat emits online/good only. Retain support for
		// the already-recognized optional empty stat object, without requiring it.
		if stat := status["stat"]; stat != nil {
			if _, e = stat.Fields(); e != nil {
				return "", true, fmt.Errorf("invalid heartbeat status.stat: %w", e)
			}
		}
	default:
		return "", true, fmt.Errorf("unsupported system event")
	}
	return kind, true, nil
}
