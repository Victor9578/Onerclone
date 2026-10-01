package app

// login.go —— v0.2.0 反馈问题③：开放任意 rclone 后端（不只夸克）。
//
// rclone 的 `config create <name> <type> --non-interactive` 是**通用状态机**
// （2026-09-28 本机 v1.70.0-quark 实测）：
//   - 逐题吐 JSON：{State, Error, Result, Option:{Name, Help, Required, Examples}}
//   - 答 `--continue --state <State> --result <值>` 推进到下一题
//   - State 为空 = 配置完成落盘
// 题型分支：
//   - OAuth 后端（dropbox 实测）：首题 `*oauth-islocal`（本机有无浏览器）→
//     答 true 后 rclone 自己开浏览器等本地回调；答 false 进 `*oauth-authorize`，
//     Help 里给 `rclone authorize "dropbox"` 指引 → 渲染成"粘贴 token"输入框
//   - 扫码后端（quark）：qr_start/qr_poll（复用现有 quark.go 渲染）
//   - 无必填项后端（local/ftp 实测）：直接落盘，State:""
// ⚠️ `config create` 会重建整个 remote 段（踩坑 #18：cookie/token 同样会丢）
//   → 全程套用 quark.go 的「原始 rclone.conf 快照 + 恢复」套路。
//
// CLI：`onerclone login`（交互式逐题作答）

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// loginQuestion 是通用状态机的一题（与 qrQuestion 同构，多了 Required）。
type loginQuestion struct {
	State  string `json:"State"`
	Error  string `json:"Error"`
	Result string `json:"Result"`
	Option struct {
		Name     string `json:"Name"`
		Help     string `json:"Help"`
		Required bool   `json:"Required"`
		Examples []struct {
			Value string `json:"Value"`
			Help  string `json:"Help"`
		} `json:"Examples"`
	} `json:"Option"`
}

// providerInfo 是 `config providers` 里一个后端的元数据。
type providerInfo struct {
	Name        string `json:"Name"`
	Description string `json:"Description"`
}

// listProviders 列出 rclone 支持的全部后端类型（`config providers`）。
// 实测输出是**顶层数组**（非 {Providers:[...]} 包装）。
func listProviders(exe string) ([]providerInfo, error) {
	out, err := rcloneOutput(exe, "config", "providers")
	if err != nil {
		return nil, fmt.Errorf("rclone config providers: %w: %s", err, oneLine(out))
	}
	var prov []struct {
		Name        string   `json:"Name"`
		Description string   `json:"Description"`
		Aliases     []string `json:"Aliases"`
	}
	if err := json.Unmarshal([]byte(out), &prov); err != nil {
		return nil, fmt.Errorf("providers 输出不是 JSON: %v", err)
	}
	out2 := make([]providerInfo, 0, len(prov))
	for _, p := range prov {
		if p.Name == "" || p.Name == "alias" || p.Name == "cache" ||
			p.Name == "crypt" || p.Name == "local" || p.Name == "http" {
			continue // 组合/本地后端不参与"登录"
		}
		out2 = append(out2, providerInfo{Name: p.Name, Description: p.Description})
	}
	sort.Slice(out2, func(i, k int) bool { return out2[i].Name < out2[k].Name })
	return out2, nil
}

// loginStart 以 --non-interactive 启动配置状态机，返回首题与恢复函数。
// 与 quarkStartQRSession 同款保护：创建前快照 [name] 段，创建后立刻补回。
func loginStart(exe, name, ptype string) (*loginQuestion, func(), error) {
	cfg := quarkRawConfigPath(exe)
	saved := quarkReadSectionRaw(cfg, name)
	restore := func() { quarkRestoreSectionRaw(cfg, name, saved) }

	c := exec.Command(exe, "config", "create", name, ptype, "--non-interactive")
	out, err := c.Output()
	if err != nil {
		restore()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, restore, fmt.Errorf("%v: %s", err, oneLine(string(ee.Stderr)))
		}
		return nil, restore, fmt.Errorf("%v: %s", err, oneLine(string(out)))
	}
	quarkRestoreSectionRaw(cfg, name, saved) // 立刻补回丢失的键
	var q loginQuestion
	if err := json.Unmarshal(out, &q); err != nil {
		return nil, restore, fmt.Errorf("rclone 输出不是 JSON: %v（%s）", err, oneLine(string(out)))
	}
	if q.Error != "" {
		return nil, restore, errors.New(q.Error)
	}
	return &q, restore, nil
}

// loginAnswer 推进状态机：回答当前题，返回下一题（State 为空 = 完成）。
func loginAnswer(exe, name, state, result string) (*loginQuestion, error) {
	c := exec.Command(exe, "config", "create", name, "--non-interactive",
		"--continue", "--state", state, "--result", result)
	out, err := c.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%v: %s", err, oneLine(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("%v: %s", err, oneLine(string(out)))
	}
	var q loginQuestion
	if err := json.Unmarshal(out, &q); err != nil {
		return nil, fmt.Errorf("rclone 输出不是 JSON: %v（%s）", err, oneLine(string(out)))
	}
	if q.Error != "" {
		return nil, errors.New(q.Error)
	}
	return &q, nil
}

// cmdLogin 通用登录 CLI：`onerclone login [name] [type]`。
// 逐题作答：普通必填题直接读 stdin；OAuth islocal 题自动答 true（rclone
// 自己开浏览器）；扫码题（quark）转交现有 quarkQRLogin 渲染二维码。
func cmdLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	rcloneExe := fs.String("rclone", "", "rclone 可执行文件（默认：配置 > exe 同目录 > PATH）")
	name := fs.String("name", "", "remote 名称（默认 = 后端类型名）")
	ptype := fs.String("type", "", "后端类型（如 dropbox / onedrive / quark；省略则列出可选）")
	_ = fs.Parse(args)
	// 纯配置命令：只读配置不写模板（避免污染发布目录）
	*rcloneExe = resolveRclone(*rcloneExe, loadConfigOpt(false).Rclone)

	if *ptype == "" {
		fmt.Println("── 支持的后端类型（rclone config providers）──")
		prov, err := listProviders(*rcloneExe)
		if err != nil {
			fmt.Printf("❌ %v\n", err)
			os.Exit(1)
		}
		for _, p := range prov {
			fmt.Printf("  %-14s %s\n", p.Name, oneLine(p.Description))
		}
		fmt.Println("\n用法: onerclone login -type dropbox [-name mydropbox]")
		return
	}
	if *name == "" {
		*name = *ptype
	}

	fmt.Printf("── 登录 rclone 后端 [%s] 类型 %s ──\n", *name, *ptype)
	q, restore, err := loginStart(*rcloneExe, *name, *ptype)
	if err != nil {
		fmt.Printf("❌ 启动配置失败: %v\n", err)
		os.Exit(1)
	}

	// 状态机循环：State 为空 = 完成
	for q != nil && q.State != "" {
		next, done, err := loginStepCLI(*rcloneExe, *name, q)
		if err != nil {
			restore()
			fmt.Printf("❌ %v\n（原 remote 参数已恢复，登录态不丢）\n", err)
			os.Exit(1)
		}
		if done {
			break
		}
		q = next
	}
	fmt.Println("── 验证连接 ──")
	if err := runRclone(*rcloneExe, "lsd", *name+":"); err != nil {
		fmt.Printf("❌ 验证失败（凭据可能未生效）: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ 登录完成：%s:（改 onerclone.json 的 remote 字段即可启用）\n", *name)
}

// loginStepCLI 处理状态机的一题（CLI 交互版）。
// 返回（下一题, 是否完成, 错误）。
func loginStepCLI(exe, name string, q *loginQuestion) (*loginQuestion, bool, error) {
	// step 推进一题并适配三返回值（State 为空 = 完成）
	step := func(state, result string) (*loginQuestion, bool, error) {
		nq, err := loginAnswer(exe, name, state, result)
		if err != nil {
			return nil, false, err
		}
		if nq == nil || nq.State == "" {
			return nil, true, nil
		}
		return nq, false, nil
	}
	switch {
	// OAuth：本机有无浏览器 → 答 true 让 rclone 自己开浏览器等回调
	case q.State == "*oauth-islocal":
		fmt.Println("检测到 OAuth 后端：rclone 将自动打开浏览器完成授权…")
		return step(q.State, "true")

	// OAuth：无浏览器场景的粘贴 token 题（CLI 走不到这里，除非用户答了 false）
	case q.State == "*oauth-authorize":
		fmt.Println("需要手动授权：")
		fmt.Println(strings.TrimSpace(q.Option.Help))
		fmt.Print("粘贴 rclone authorize 输出的结果: ")
		var token string
		if _, err := fmt.Scanln(&token); err != nil {
			return nil, false, err
		}
		return step(q.State, token)

	// quark 扫码：转交现有渲染（quarkQRLogin 内部处理 qr_start/qr_poll）
	case q.State == "qr_start" || q.State == "qr_poll":
		quarkQRLogin(exe, name, 1)
		return nil, true, nil

	// 普通必填题：展示帮助与示例，读 stdin
	default:
		if q.Option.Help != "" {
			fmt.Println(strings.TrimSpace(q.Option.Help))
		}
		for _, ex := range q.Option.Examples {
			fmt.Printf("  示例: %s\n", ex.Value)
		}
		prompt := q.Option.Name
		if prompt == "" {
			prompt = q.State
		}
		fmt.Printf("%s: ", prompt)
		var val string
		if _, err := fmt.Scanln(&val); err != nil && err.Error() != "unexpected newline" {
			return nil, false, err
		}
		if val == "" && q.Option.Required {
			return nil, false, fmt.Errorf("%s 是必填项", q.Option.Name)
		}
		return step(q.State, val)
	}
}
