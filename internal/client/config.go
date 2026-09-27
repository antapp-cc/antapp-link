package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/antapp-cc/antapp-link/internal/pki"
)

// LoadInvite 接受三种输入：单行连接码本身、连接码 JSON 文件、存着单行码的文本文件。
// 用户从聊天窗口复制的是第一种，从服务端带回来的是第二种，两种都会碰到。
func LoadInvite(pathOrCode string) (pki.Invite, error) {
	trimmed := strings.TrimSpace(pathOrCode)
	if trimmed == "" {
		return pki.Invite{}, fmt.Errorf("连接码为空")
	}
	if strings.HasPrefix(trimmed, pki.Scheme) {
		return pki.Decode(trimmed)
	}

	raw, err := os.ReadFile(trimmed)
	if err != nil {
		return pki.Invite{}, fmt.Errorf("既不是 %s 开头的连接码，也读不到这个文件: %w", pki.Scheme, err)
	}
	if text := strings.TrimSpace(string(raw)); strings.HasPrefix(text, pki.Scheme) {
		return pki.Decode(text)
	}

	var inv pki.Invite
	if err := json.Unmarshal(raw, &inv); err != nil {
		return pki.Invite{}, fmt.Errorf("连接码文件既不是 %s 也不是合法 JSON: %w", pki.Scheme, err)
	}
	if err := inv.Validate(); err != nil {
		return pki.Invite{}, err
	}
	return inv, nil
}

// InviteFileName 是连接码在 config\ 下的名字。
//
// 用 .antapp 而不是通用的 .conf：这个后缀已经关联到客户端，用户想手动换连接码时
// 直接双击这个文件就行 —— 跟从别处拿到的连接码文件是同一种东西，没必要两套命名。
const InviteFileName = "node.antapp"

// InviteFilePath 是客户端保存连接码的位置。
func InviteFilePath(root string) string { return filepath.Join(ConfigDir(root), InviteFileName) }

// SaveInvite 把连接码存下来，这样下次启动不用再导入一次。
func SaveInvite(root string, inv pki.Invite) error {
	code, err := inv.Encode()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ConfigDir(root), 0o700); err != nil {
		return err
	}
	// 内含私钥，权限收紧
	return os.WriteFile(InviteFilePath(root), []byte(code+"\n"), 0o600)
}

func LoadSavedInvite(root string) (pki.Invite, error) {
	return LoadInvite(InviteFilePath(root))
}
