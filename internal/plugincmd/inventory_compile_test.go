package plugincmd

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
)

var (
	_ = sdk.SuppressEvent
	_ = sdk.EmitEvents
	_ = sdk.EmitAssembledToolCall
	_ = sdk.ReplaceToolArguments
	_ = sdk.SuppressToolCall
	_ = sdk.ReplaceText
	_ = sdk.SuppressText
	_ = sdk.ModelComplete
	_ = sdk.GetModelPricing
	_ = sdk.SetIdentity
	_ = sdk.MetaGet
	_ = sdk.MetaSet
	_ = sdk.CacheGet
	_ = sdk.CacheSet
	_ = sdk.SharedCacheGet
	_ = sdk.SharedCacheSet
	_ = sdk.PassEvent
	_ = sdk.PassToolCall
	_ = sdk.PassText
	_ = sdk.PassRequest
	_ = sdk.PassResponse
	_ = sdk.NewStreamHandler
	_ = sdk.NewStreamAssembler
	_ = (*sdk.StreamHandler).OnToolCall
	_ = (*sdk.StreamHandler).OnTextDelta
	_ = (*sdk.StreamHandler).Register
	_ = (*sdk.StreamAssembler).WithToolAssembly
	_ = (*sdk.StreamAssembler).Feed
	_ = sdk.OnStreamChunk
)

var _ = func(handler *sdk.StreamHandler) sdk.TextAction {
	return sdk.PassText()
}

var _ context.Context
