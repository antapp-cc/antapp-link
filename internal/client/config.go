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

// ImportInviteFile 把用户拿来的连接码文件原样复制进 config\（保留原文件名）。
// 同名文件直接覆盖 —— 用户把同一个名字的文件换成新内容时，意图就是替换。
// 返回落地的文件名。
func ImportInviteFile(root, srcPath string) (string, error) {
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return "", err
	}
	return ImportInviteBytes(root, filepath.Base(srcPath), raw)
}

// ImportInviteBytes 把连接码内容以给定文件名写进 config\（剪贴板导入没有文件形态，用这个落盘）。
func ImportInviteBytes(root, name string, raw []byte) (string, error) {
	if err := os.MkdirAll(ConfigDir(root), 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(ConfigDir(root), name)
	// 内含私钥，权限收紧
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		return "", err
	}
	return name, nil
}

// ListInviteFiles 列出 config\ 里的全部 .antapp 文件名（按名字排序）。
// 只看文件名，不读内容 —— 文件名就是配置的身份，连接时才读选中的那个。
func ListInviteFiles(root string) []string {
	entries, err := os.ReadDir(ConfigDir(root))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".antapp") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// LoadInviteFile 按 config\ 里的文件名加载连接码。
func LoadInviteFile(root, name string) (pki.Invite, error) {
	return LoadInvite(filepath.Join(ConfigDir(root), name))
}

// NameDup 是一组同名的连接码文件。
type NameDup struct {
	Name  string   // 去掉 Windows 复制序号后的原始名
	Files []string // 同名的文件名列表
}

// DuplicateFileNames 找出同名的文件组。
//
// 云服 invite 出的固定叫 pinode.antapp，用户复制几份进来就成了
// pinode (2).antapp、pinode (3).antapp —— 去掉 Windows 的复制序号后名字相同，
// 光看名字分不清谁是谁，必须提醒用户自己挑。
func DuplicateFileNames(names []string) []NameDup {
	groups := map[string][]string{}
	var order []string
	for _, n := range names {
		k := normalizeCopyName(n)
		if groups[k] == nil {
			order = append(order, k)
		}
		groups[k] = append(groups[k], n)
	}
	var out []NameDup
	for _, k := range order {
		if len(groups[k]) > 1 {
			out = append(out, NameDup{Name: k, Files: groups[k]})
		}
	}
	return out
}

// normalizeCopyName 去掉 Windows 复制粘贴加的「 (2)」序号：pinode (2).antapp → pinode.antapp。
func normalizeCopyName(name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for {
		i := strings.LastIndex(base, "(")
		if i < 0 || !strings.HasSuffix(base, ")") {
			break
		}
		inner := base[i+1 : len(base)-1]
		if inner == "" {
			break
		}
		allDigits := true
		for _, r := range inner {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if !allDigits {
			break
		}
		base = strings.TrimSpace(base[:i])
	}
	return base + ext
}

// HasMultipleInvites 报告 config\ 里是否有两个及以上的连接码文件。
// 有就必须让用户挑，客户端不该自作主张用哪一个。
func HasMultipleInvites(root string) bool {
	return len(ListInviteFiles(root)) >= 2
}
