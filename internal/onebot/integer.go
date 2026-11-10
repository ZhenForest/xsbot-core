package onebot

import (
	"github.com/ZhenForest/xsbot-core/internal/message"
	"github.com/ZhenForest/xsbot-core/internal/strictjson"
)

func integer(v *strictjson.Value, allowString bool) (int64, error) {
	return message.Int(v, allowString)
}
