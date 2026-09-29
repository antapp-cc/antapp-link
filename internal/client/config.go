package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
// 名字与云服 invite 出的文件保持一致（pinode.antapp），用户复制过来不用改任何东西。
const InviteFileName = "pinode.antapp"

// legacyInviteFileName 是 0.2.0 之前的生效配置名，启动时迁移到新名字。
const legacyInviteFileName = "node.antapp"

// MigrateInviteFileName 把旧布局的 node.antapp 迁移成 pinode.antapp（只发生一次）。
// 新名字已存在时不迁移 —— 那说明用户已经在新布局上工作，旧文件留作候选即可。
func MigrateInviteFileName(root string) {
	old := filepath.Join(ConfigDir(root), legacyInviteFileName)
	if _, err := os.Stat(old); err != nil {
		return
	}
	if _, err := os.Stat(InviteFilePath(root)); err == nil {
		return
	}
	_ = os.Rename(old, InviteFilePath(root))
}

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

// Candidate 是 config\ 里发现的一个候选连接码文件（不含当前生效的 node.antapp）。
type Candidate struct {
	File string
	Inv  pki.Invite
}

// ScanInvites 扫描 config\ 下除生效配置外的全部 .antapp 文件。
//
// 解析失败的条目安静地跳过 —— 用户可能把别的东西也拖进这个文件夹，
// 为它报错弹窗比直接无视更吵。
func ScanInvites(root string) []Candidate {
	entries, err := os.ReadDir(ConfigDir(root))
	if err != nil {
		return nil
	}
	var out []Candidate
	for _, e := range entries {
		if e.IsDir() || e.Name() == InviteFileName || !strings.EqualFold(filepath.Ext(e.Name()), ".antapp") {
			continue
		}
		inv, err := LoadInvite(filepath.Join(ConfigDir(root), e.Name()))
		if err != nil {
			continue
		}
		out = append(out, Candidate{File: e.Name(), Inv: inv})
	}
	return out
}

// DuplicateNames 返回候选里出现两次及以上的客户端名。两个文件的节点名相同时
// 没法凭名字区分谁是谁（服务端重签、手工复制都会造成），必须提醒用户自己挑。
func DuplicateNames(cands []Candidate) []string {
	count := map[string]int{}
	for _, c := range cands {
		count[c.Inv.Name]++
	}
	var dups []string
	for name, n := range count {
		if n > 1 {
			dups = append(dups, name)
		}
	}
	sort.Strings(dups)
	return dups
}
