package sdk

import (
	"context"
	"errors"
)

const MaxDocumentBytes = 8 << 20
const MaxDocumentSelectorBytes = 16 << 10

// DocumentEditable is optional and independent of Configurable. These callbacks
// may overlap Deliver, other management calls and Close. Implementations own
// validation, storage and cancellation. No filesystem paths cross this API.
type DocumentEditable interface {
	DocumentEditors(context.Context) ([]DocumentEditor, error)
	ReadDocument(ctx context.Context, editor, input string) (Document, error)
	SaveDocument(ctx context.Context, editor, key, revision, content string) (Document, error)
}

// Fields select a document, not plugin configuration. Supported selector kinds
// are text, number and checkbox. MaxBytes must be in 1..MaxDocumentBytes.
type DocumentEditor struct {
	ID       string               `json:"id"`
	Label    string               `json:"label"`
	Hint     string               `json:"hint"`
	Format   string               `json:"format"` // text or json; plugin validates semantics
	MaxBytes int                  `json:"max_bytes"`
	Confirm  string               `json:"confirm"`
	Fields   []ConfigurationField `json:"fields"`
}

// Key and Revision are bounded non-secret ASCII identifiers, not paths or
// credentials. Content remains UTF-8 text even for JSON (no JS number coercion).
// Save must compare Revision under the same synchronization as commit, and
// return the committed text and its revision. A failed save must not claim a
// commit was rolled back if the result is uncertain.
type Document struct {
	Key      string
	Revision string
	Content  string
}

var ErrDocumentConflict = errors.New("文档已变化，请重新读取并核对；编辑内容已保留")
