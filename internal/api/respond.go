package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"nearsend/internal/protocol"
	"nearsend/internal/transfer"
)

// maxJSONBody 普通 JSON 请求体上限。分块上传走独立路径，不受此限制。
const maxJSONBody = 2 << 20

// apiError 是统一错误响应体。
type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// statusForCode 把稳定错误码映射为 HTTP 状态码。
func statusForCode(code string) int {
	switch code {
	case protocol.CodeNotFound:
		return http.StatusNotFound
	case protocol.CodeForbidden:
		return http.StatusForbidden
	case protocol.CodeUnauthorized, protocol.CodeSessionExpired:
		return http.StatusUnauthorized
	case protocol.CodeBadRequest, protocol.CodeInvalidChunkIndex, protocol.CodeInvalidFileName,
		protocol.CodePathTraversal, protocol.CodeInvalidState, protocol.CodeChunkHashMismatch,
		protocol.CodeSourceChanged:
		return http.StatusBadRequest
	case protocol.CodeFileTooLarge, protocol.CodeTaskTooLarge:
		return http.StatusRequestEntityTooLarge
	case protocol.CodeConflictExists:
		return http.StatusConflict
	case protocol.CodeDiskFull:
		return http.StatusInsufficientStorage
	case protocol.CodeTooManyConcurrent:
		return http.StatusTooManyRequests
	case protocol.CodeDropboxExpired, protocol.CodeNotResumable:
		return http.StatusGone
	default:
		return http.StatusInternalServerError
	}
}

// writeJSON 写出成功响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeErr 写出错误响应。
//
// 只把稳定错误码与面向用户的中文文案返回给客户端；内部细节（含绝对路径、
// 数据库语句、堆栈）写进服务端日志，绝不进入响应体。
func writeErr(w http.ResponseWriter, log *slog.Logger, err error) {
	code := transfer.CodeOf(err)
	msg := protocol.Message[code]
	if msg == "" {
		msg = protocol.Message[protocol.CodeInternal]
	}
	var te *transfer.Error
	if errors.As(err, &te) && te.Msg != "" {
		msg = te.Msg
	}
	if log != nil {
		log.Warn("api error", "code", code, "detail", err.Error())
	}
	var body apiError
	body.Error.Code = code
	body.Error.Message = msg
	writeJSON(w, statusForCode(code), body)
}

// readJSON 解析请求体，拒绝未知字段与超大体积。
func readJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.Body == nil {
		return transfer.NewError(protocol.CodeBadRequest, "empty body")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return transfer.NewError(protocol.CodeBadRequest, "empty body")
		}
		return transfer.NewError(protocol.CodeBadRequest, err.Error())
	}
	// 拒绝尾随内容，避免同一请求被解析出两段语义。
	if dec.More() {
		return transfer.NewError(protocol.CodeBadRequest, "unexpected trailing content")
	}
	return nil
}

// queryInt 解析整数查询参数，缺省值由 def 提供。
func queryInt(r *http.Request, key string, def, min, max int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min {
		return def
	}
	if max > 0 && v > max {
		return max
	}
	return v
}

// queryInt64 解析 64 位整数查询参数。
func queryInt64(r *http.Request, key string, def int64) int64 {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}

// bearerToken 从 Authorization 头提取会话令牌。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}
