package client

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// rotatingLog 是日志文件写入器（不轮转）：一次运行内的日志完整保留，重启/重连
// 时清空，避免新旧混在一起。
type rotatingLog struct {
	path string

	mu   sync.Mutex
	file *os.File
}

func OpenLog(path string) (*rotatingLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &rotatingLog{path: path}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *rotatingLog) open() error {
	// 打开即清空：重启/重连后日志从零开始，不跟旧的混在一起。
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	l.file = f
	return nil
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return 0, os.ErrClosed
	}
	return l.file.Write(p)
}

// SwitchFile 把日志切到新文件。文件是追加写的（保留全部历史）；
// 界面缓冲由调用方清空，界面只显示最近一段。
func (l *rotatingLog) SwitchFile(path string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	l.path = path
	// 新会话从零开始：清空旧会话的日志（软件重启/重新连接时不跟旧的混在一起）。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	l.file = f
	return nil
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func (l *rotatingLog) Path() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.path
}

// LogBuffer 保留最近若干行日志供界面显示。
//
// 界面不去 tail 日志文件：文件是按大小轮转的，读它还要处理并发和轮转；
// 而在写日志时顺手留一份几乎不花成本。
type LogBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
	// seq 是累计写入过的行数，只增不减。界面靠它算出「上次看到哪了」，
	// 从而只追加新行 —— 整体 SetText 会让只读框变成全选，能不用就不用。
	seq uint64
}

func NewLogBuffer(max int) *LogBuffer {
	if max <= 0 {
		max = 200
	}
	return &LogBuffer{max: max}
}

func (b *LogBuffer) addLine(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, line)
	b.seq++
	if len(b.lines) > b.max {
		keep := make([]string, b.max)
		copy(keep, b.lines[len(b.lines)-b.max:])
		b.lines = keep
	}
}

// Since 返回序号 >= seq 的那些行，以及新的序号。
//
// 界面用它做增量追加。seq 太旧（那些行已经被轮转丢掉）时从现有最早一行开始，
// 调用方拿到的是「现在缓冲区里的全部内容」。
func (b *LogBuffer) Since(seq uint64) ([]string, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	start := b.seq - uint64(len(b.lines)) // 缓冲区里第一行的序号
	if seq < start {
		seq = start
	}
	out := make([]string, 0, b.seq-seq)
	if idx := int(seq - start); idx < len(b.lines) {
		out = append(out, b.lines[idx:]...)
	}
	return out, b.seq
}

// Clear 清空界面缓冲：新连接会话的界面日志同样从零开始。
func (b *LogBuffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = nil
}

// Seq 返回累计写入过的行数；窗口重排后界面用它把增量游标推到最新。
func (b *LogBuffer) Seq() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// Tail 返回最后 n 行。n<=0 表示全部。
func (b *LogBuffer) Tail(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n > len(b.lines) {
		n = len(b.lines)
	}
	out := make([]string, n)
	copy(out, b.lines[len(b.lines)-n:])
	return out
}

// teeHandler 让同一份日志有两个去处：文件里保留结构化原文（排查时字段好搜），
// 界面上给一行人类读得懂的短格式（时间 + 级别 + 消息 + 字段）。
//
// 直接在界面里显示 slog 的原文也行，但那样满屏都是 time=/level=/msg=，
// 跟用户已经习惯的 OpenVPN 客户端日志观感差得远。
type teeHandler struct {
	w     io.Writer
	buf   *LogBuffer
	attrs []slog.Attr
}

// Enabled 放行所有级别：Debug 也要进 Handle 写进文件（补拨失败、丢包、UDP 失败等
// 关键现场都在 Debug 里），界面显示与否由 Handle 决定。
func (h *teeHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *teeHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	// 级别符号用 ASCII（. / ! / x）而不是 ·▲×，后者在等宽字体里占两格。
	b.WriteString(levelTag(r.Level))
	b.WriteString(" ")
	b.WriteString(r.Message)
	for _, a := range h.attrs {
		b.WriteString(" ")
		b.WriteString(attrKeyLabel(a.Key))
		b.WriteString("=")
		b.WriteString(a.Value.String())
	}
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" ")
		b.WriteString(attrKeyLabel(a.Key))
		b.WriteString("=")
		b.WriteString(a.Value.String())
		return true
	})
	// 一条日志就是一行：底层错误里偶尔带 \n，换成空格。日志区已经关掉自动折行
	// （见 ui_windows.go），超长内容由横向滚动条承担 —— 和 OpenVPN 一样。
	body := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(b.String())
	// 界面只显示到月日（省宽度）；文件带年份（跨年/长期回看时必要）。
	uiLine := r.Time.Format("01-02 15:04:05") + " " + body
	fileLine := r.Time.Format("2006-01-02 15:04:05") + " " + body
	// 文件写全部级别（含 Debug，排查现场靠它），不截断；界面只显示 Info 及以上。
	if _, err := io.WriteString(h.w, fileLine+"\n"); err != nil {
		return err
	}
	if r.Level >= slog.LevelInfo {
		// 存完整行：界面在展示时按当前窗口宽度截断（窗口拉宽后旧行也能重排），
		// 文件本来就是完整的。
		h.buf.addLine(uiLine)
	}
	return nil
}

// logLineMaxUnits 是界面日志区一行能容纳的显示宽度（ASCII=1，中文=2）。
//
// 取 70：约 490px，接近日志区客户区宽度（约 500px）；超过的部分截断显示 …，
// 完整内容看日志文件。之前 75（约 525px）截断后仍超宽，EDIT 控件会折成两行。
const logLineMaxUnits = 70

// ellipsisUnits 是截断标记 … 的显示宽度。
const ellipsisUnits = 2

// fitLogLine 保证一行放得下，超出部分截断并以 … 结尾。
//
// 为什么不靠控件自己处理：给 Edit 控件加 WS_HSCROLL + ES_AUTOHSCROLL + 发
// EM_SETTARGETDEVICE 之后，样式确认是生效的，但实测超宽内容它仍然会折行，而且
// 折出来的续行顶格，正好压在日期时间那一列上 —— 用户反复提的就是这个问题。
//
// 所以从数据层保证：界面只展示放得下的部分。完整内容在日志文件里
// （file handler 不经过这里），需要时去看文件。
func fitLogLine(s string) string {
	return fitLogLineTo(s, logLineMaxUnits)
}

// fitLogLineTo 按给定显示宽度截断；界面会按当前窗口宽度传入 max。
func fitLogLineTo(s string, max int) string {
	width := 0
	for i, r := range s {
		w := 1
		if r > 127 {
			w = 2
		}
		// 截断用的 … 自己占 2 格（中文宽度），必须提前预留，否则截出来还是超
		if width+w+ellipsisUnits > max {
			return s[:i] + "…"
		}
		width += w
	}
	return s
}

// attrKeyLabel 把 slog 的英文 key 换成中文。
//
// 界面是给用户看的，`err=`、`reason=`、`slot=` 这些字面对他们没有意义。日志文件
// 那边保持原样（英文 key 好 grep），只有界面缓冲区走这个映射。
// 翻不出来的保持原样 —— 宁可露出一个陌生单词，也不要编一个错误的名字。
func attrKeyLabel(key string) string {
	if label, ok := attrKeyLabels[key]; ok {
		return label
	}
	return key
}

var attrKeyLabels = map[string]string{
	"err":       "错误",
	"reason":    "原因",
	"local":     "本端",
	"remote":    "对端",
	"member":    "成员",
	"slot":      "槽位",
	"live":      "存活",
	"drops":     "累计丢弃",
	"sources":   "更新源数",
	"goroutine": "协程数",
	"heap_mb":   "堆MB",
	"rx_mb":     "接收MB",
	"tx_mb":     "发送MB",
	"pkt_drops": "上行丢弃",
	"attempt":   "第几次",
	"retry_in":  "重试间隔",
	"waited":    "已等",
	"version":   "版本",
	"current":   "当前",
	"latest":    "最新",
	"bytes":     "字节",
	"path":      "路径",
	"file":      "文件",
	"target":    "目标",
	"port":      "端口",
	"client":    "客户端",
	"tunnel_ip": "隧道IP",
	"udp_port":  "UDP端口",
	"members":   "连接数",
	"server":    "服务端",
	"new":       "新",
	"sid":       "会话号",
	"ms":        "毫秒",
	"port_test": "端口测试",
	// 下面这几个漏了很久：watchRunStats 打的是 "goroutines"（复数），
	// 而表里只有 "goroutine"，于是「运行状态」那行一直显示英文 goroutines=25。
	"goroutines":  "协程数",
	"captured_at": "记录于",
	"did_resume":  "会话复用",
	"len":         "长度",
	"max":         "上限",
	"idle":        "空闲",
	// 丢包分类：一个总数涨上去分不清是哪一层丢的，四类必须分开读。
	"drop_pktch":  "队列满",
	"drop_empty":  "空槽",
	"drop_queue":  "槽队列",
	"drop_member": "摘槽",
}

// WithAttrs / WithGroup 在本项目里用不到（都是直接 slog.Info/Warn/Error），
// 但 slog.Handler 要求实现，这里保持语义正确即可。
func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cp := *h
	cp.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &cp
}

func (h *teeHandler) WithGroup(string) slog.Handler { return h }

// levelTag 用符号而不是「信息/警告/错误」四个字：每条都印一遍的话，
// 那两个字占了时间戳之后最醒目的位置，而绝大多数行都是「信息」，纯属噪声。
// 用 ASCII 符号（. ! x）而不是 ·▲×：后者在等宽字体里占两格宽度，而日志区一行只有
func levelTag(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "x"
	case l >= slog.LevelWarn:
		return "!"
	default:
		return "."
	}
}

// sessionLogger = 文件日志 + 界面缓冲：每次连接会话切换文件并清空。
type sessionLogger struct {
	rot *rotatingLog
	buf *LogBuffer
}

// activeSession 客户端单实例，包内单例安全。NewFileLogger 时赋值。
var activeSession *sessionLogger

var rootDirForLogs string

// logResetEpoch 每次日志会话切换（启动/连接/切配置）时自增；界面靠它判断
// 要不要把日志控件清空，避免旧会话的日志和新会话的混在一起。
var logResetEpoch atomic.Uint64

// LogResetEpoch 返回当前日志会话代数；界面检测到变化就清空日志控件。
func LogResetEpoch() uint64 { return logResetEpoch.Load() }

// SwitchLogSession 把日志切到与配置文件同名的会话文件（pinode.antapp → pinode.log）。
// 文件清空（新会话从零开始），界面缓冲清空并通知界面清空日志控件。
// 失败安静忽略：日志切换失败不该挡住连接。
func SwitchLogSession(source string) {
	if activeSession == nil {
		return
	}
	name := strings.TrimSuffix(source, ".antapp")
	if name == "" {
		name = "client"
	}
	_ = activeSession.rot.SwitchFile(filepath.Join(LogsDir(rootDirForLogs), name+".log"))
	// client.log 只装启动瞬间的过渡日志（自愈/更新检查等，发生在选定配置之前），
	// 会话已切走，它没有保留价值——删掉，别在 logs 目录里留残渣
	_ = os.Remove(filepath.Join(LogsDir(rootDirForLogs), "client.log"))
	activeSession.buf.Clear()
	logResetEpoch.Add(1)
}

// CurrentLogFile 返回当前会话日志文件的路径，供界面的「查看日志文件」使用。
// 还没有日志（启动早期）时返回空串。
func CurrentLogFile() string {
	if activeSession == nil || activeSession.rot == nil {
		return ""
	}
	return activeSession.rot.Path()
}

// NewFileLogger 建一个同时写文件与界面缓冲的 logger。
func NewFileLogger(root string) (*slog.Logger, *LogBuffer, func(), error) {
	rootDirForLogs = root
	rot, err := OpenLog(filepath.Join(LogsDir(root), "client.log"))
	if err != nil {
		return nil, nil, nil, err
	}
	buf := NewLogBuffer(500)
	activeSession = &sessionLogger{rot: rot, buf: buf}
	return slog.New(&teeHandler{w: rot, buf: buf}), buf,
		func() { _ = rot.Close() }, nil
}
