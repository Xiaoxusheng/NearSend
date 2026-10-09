package api

import (
	qrcode "github.com/skip2/go-qrcode"

	"nearsend/internal/protocol"
	"nearsend/internal/transfer"
)

// qrPNG 生成指定内容的二维码 PNG。
func qrPNG(content string, size int) ([]byte, error) {
	if content == "" {
		return nil, transfer.NewError(protocol.CodeBadRequest, "empty qr content")
	}
	if size < 128 {
		size = 128
	}
	if size > 1024 {
		size = 1024
	}
	// 中等纠错级别：在屏幕显示场景下容错与密度平衡最好。
	return qrcode.Encode(content, qrcode.Medium, size)
}
