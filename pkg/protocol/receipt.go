package protocol

import "encoding/json"

// ReceiptPayload type tags (the schema discriminator).
const (
	ReceiptStartResult       = "StartResult"
	ReceiptResumeResult      = "ResumeResult"
	ReceiptInputReceipt      = "InputReceipt"
	ReceiptCompactResult     = "CompactResult"
	ReceiptSessionOpenResult = "SessionOpenResult"
	ReceiptForkResult        = "ForkResult"
	ReceiptInterruptReceipt  = "InterruptReceipt"
	ReceiptRunResult         = "RunResult"
)

func wrapReceipt[T Message](tag string, v T) (ReceiptPayload, error) {
	raw, err := Encode(v)
	if err != nil {
		return ReceiptPayload{}, err
	}
	return ReceiptPayload{Type: tag, Value: json.RawMessage(raw)}, nil
}

// NewReceiptPayload wraps one operation result in the tagged union a receipt
// stores. v must be one of the eight result types, by value; its canonical JSON
// becomes Value and is validated on the way.
func NewReceiptPayload(v any) (ReceiptPayload, error) {
	switch x := v.(type) {
	case StartResult:
		return wrapReceipt(ReceiptStartResult, x)
	case ResumeResult:
		return wrapReceipt(ReceiptResumeResult, x)
	case InputReceipt:
		return wrapReceipt(ReceiptInputReceipt, x)
	case CompactResult:
		return wrapReceipt(ReceiptCompactResult, x)
	case SessionOpenResult:
		return wrapReceipt(ReceiptSessionOpenResult, x)
	case ForkResult:
		return wrapReceipt(ReceiptForkResult, x)
	case InterruptReceipt:
		return wrapReceipt(ReceiptInterruptReceipt, x)
	case RunResult:
		return wrapReceipt(ReceiptRunResult, x)
	}
	return ReceiptPayload{}, requestError("value is not a receipt result type")
}

// StartResult returns the value of a StartResult receipt.
func (p ReceiptPayload) StartResult() (StartResult, error) {
	if p.Type != ReceiptStartResult {
		return StartResult{}, requestError("receipt holds a different result type")
	}
	return Decode[StartResult](p.Value)
}
