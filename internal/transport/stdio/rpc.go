package stdio

import (
	"encoding/json"
	"errors"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Standard JSON-RPC error codes, and the one code every domain error uses.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeUnknownMethod  = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeDomain         = -32000
)

// rpcErrorObject is the error member of a response. Data carries the ErrorInfo of
// the protocol where there is one.
type rpcErrorObject struct {
	Code    int                 `json:"code"`
	Message string              `json:"message"`
	Data    *protocol.ErrorInfo `json:"data,omitempty"`
}

// responseFrame is a successful response: the request's id and the canonical
// result bytes.
func responseFrame(id string, result []byte) []byte {
	out := make([]byte, 0, len(result)+len(id)+48)
	out = append(out, `{"jsonrpc":"2.0","id":`...)
	idJSON, _ := json.Marshal(id)
	out = append(out, idJSON...)
	out = append(out, `,"result":`...)
	out = append(out, result...)
	return append(out, '}')
}

// errorFrame is an error response. id is nil when the request's id could not be
// established, which JSON-RPC answers with null.
func errorFrame(id *string, e rpcErrorObject) []byte {
	var idJSON any
	if id != nil {
		idJSON = *id
	}
	raw, err := json.Marshal(struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      any            `json:"id"`
		Error   rpcErrorObject `json:"error"`
	}{"2.0", idJSON, e})
	if err != nil {
		// A fixed struct of strings and numbers cannot fail to encode.
		panic("stdio: error frame cannot be encoded: " + err.Error())
	}
	return raw
}

// toRPCError maps a Service failure to its JSON-RPC error. A *protocol.Error keeps
// its code: INVALID_PARAMS is -32602, INVALID_REQUEST is -32600, INTERNAL is -32603
// and every other code (BUSY, FORBIDDEN, REVISION_CONFLICT, IDEMPOTENCY_CONFLICT,
// UNSUPPORTED_CONTRACT, INVALID_RANGE, INTEGRITY_BLOCKED, PERSISTENCE_UNCERTAIN, ...)
// is a domain error, -32000. Anything else is an internal error whose text is never
// sent: it may hold a path or a value.
func toRPCError(err error) rpcErrorObject {
	var pe *protocol.Error
	if !errors.As(err, &pe) {
		info := protocol.ErrorInfo{Code: protocol.CodeInternal, Message: "internal error"}
		return rpcErrorObject{Code: codeInternal, Message: info.Message, Data: &info}
	}
	info := pe.Info()
	code := codeDomain
	switch pe.Code {
	case protocol.CodeInvalidParams:
		code = codeInvalidParams
	case protocol.CodeInvalidRequest:
		code = codeInvalidRequest
	case protocol.CodeInternal:
		code = codeInternal
	}
	return rpcErrorObject{Code: code, Message: info.Message, Data: &info}
}
