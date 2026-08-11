package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// runTUI drives a simple menu-driven interface over the existing commands.
//
// Credentials are resolved once here (the AccessKey prompt) and cached, so
// every action in the session reuses them instead of asking again. It builds
// the same argument slices the CLI parses and calls the same cmd* functions,
// so there is no second copy of any command's logic.
func runTUI() error {
	cfg, _, err := resolveAdminCreds()
	if err != nil {
		return err
	}
	cachedCreds = cfg

	in := bufio.NewReader(os.Stdin)
	ask := func(p string) string {
		fmt.Print(p)
		s, err := in.ReadString('\n')
		if err != nil {
			return ""
		}
		return strings.TrimSpace(s)
	}
	run := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "  error:", err)
		}
	}

	for {
		fmt.Printf("\n==== AI Env Mgr · %s ====\n", cfg.Bucket)
		fmt.Println("  1) 机器状态")
		fmt.Println("  2) 员工管理")
		fmt.Println("  3) 代员工登录")
		fmt.Println("  4) 机器管理")
		fmt.Println("  5) 封禁策略")
		fmt.Println("  6) 会话采集")
		fmt.Println("  7) 文件传输")
		fmt.Println("  8) agent 更新")
		fmt.Println("  q) 退出")
		switch ask("选择: ") {
		case "1":
			run(cmdStatus())
		case "2":
			tuiUser(ask, run)
		case "3":
			u := ask("员工名: ")
			tool := ask("工具 codex/claude/all [all]: ")
			if tool == "" {
				tool = "all"
			}
			if u != "" {
				run(cmdLogin([]string{"--user", u, "--tool", tool}))
			}
		case "4":
			tuiMachine(ask, run)
		case "5":
			tuiPolicy(ask, run)
		case "6":
			tuiCollect(ask, run)
		case "7":
			tuiFile(ask, run)
		case "8":
			tuiAgent(ask, run)
		case "q", "Q", "":
			fmt.Println("bye")
			return nil
		default:
			fmt.Println("  无效选择")
		}
	}
}

func tuiUser(ask func(string) string, run func(error)) {
	fmt.Println("  员工:  1)列表  2)新增  3)停用  4)启用")
	switch ask("  选择: ") {
	case "1":
		run(cmdUser([]string{"list"}))
	case "2":
		name := ask("  员工名: ")
		if name == "" {
			return
		}
		args := []string{"add", name}
		if c := ask("  codex 邮箱(备注,可空): "); c != "" {
			args = append(args, "--codex", c)
		}
		if c := ask("  claude 邮箱(备注,可空): "); c != "" {
			args = append(args, "--claude", c)
		}
		run(cmdUser(args))
	case "3":
		if name := ask("  员工名: "); name != "" {
			run(cmdUser([]string{"disable", name}))
		}
	case "4":
		if name := ask("  员工名: "); name != "" {
			run(cmdUser([]string{"enable", name}))
		}
	}
}

func tuiMachine(ask func(string) string, run func(error)) {
	fmt.Println("  机器:  1)列表  2)绑定  3)解绑  4)删除(下线机器)  5)看日志")
	switch ask("  选择: ") {
	case "1":
		run(cmdMachine([]string{"list"}))
	case "5":
		if h := ask("  主机名: "); h != "" {
			run(cmdLog([]string{h}))
		}
	case "2":
		h := ask("  主机名: ")
		u := ask("  员工名: ")
		if h == "" || u == "" {
			return
		}
		args := []string{"bind", h, "--user", u}
		if note := ask("  备注(可空): "); note != "" {
			args = append(args, "--note", note)
		}
		run(cmdMachine(args))
	case "3":
		if h := ask("  主机名: "); h != "" {
			run(cmdMachine([]string{"unbind", h}))
		}
	case "4":
		if h := ask("  主机名: "); h != "" {
			run(cmdMachine([]string{"forget", h}))
		}
	}
}

func tuiPolicy(ask func(string) string, run func(error)) {
	fmt.Println("  封禁:  1)查看  2)加站点  3)删站点  4)开启  5)关闭  6)同步间隔")
	switch ask("  选择: ") {
	case "1":
		run(cmdListSites())
	case "2":
		if d := ask("  域名(空格分隔): "); d != "" {
			run(cmdAddSite(strings.Fields(d)))
		}
	case "3":
		if d := ask("  域名(空格分隔): "); d != "" {
			run(cmdRemoveSite(strings.Fields(d)))
		}
	case "4":
		run(cmdSetBlock(true))
	case "5":
		run(cmdSetBlock(false))
	case "6":
		if m := ask("  分钟: "); m != "" {
			run(cmdSetInterval([]string{m}))
		}
	}
}

func tuiCollect(ask func(string) string, run func(error)) {
	fmt.Println("  采集:  1)统计  2)开启  3)关闭")
	switch ask("  选择: ") {
	case "1":
		run(cmdCollect([]string{"stat"}))
	case "2":
		args := []string{"enable"}
		if s := ask("  起始日期 YYYY-MM-DD(可空): "); s != "" {
			args = append(args, "--since", s)
		}
		if q := ask("  去抖秒数(可空): "); q != "" {
			args = append(args, "--quiet", q)
		}
		run(cmdCollect(args))
	case "3":
		run(cmdCollect([]string{"disable"}))
	}
}

func tuiAgent(ask func(string) string, run func(error)) {
	fmt.Println("  agent更新:  1)当前目标  2)发布新版  3)取消(急停)")
	switch ask("  选择: ") {
	case "1":
		run(cmdAgent([]string{"status"}))
	case "2":
		p := ask("  新 agent.exe 路径: ")
		v := ask("  版本号 (如 1.2.0): ")
		if p != "" && v != "" {
			run(cmdAgent([]string{"publish", p, "--version", v}))
		}
	case "3":
		run(cmdAgent([]string{"cancel"}))
	}
}

func tuiFile(ask func(string) string, run func(error)) {
	fmt.Println("  文件:  1)列表  2)上传  3)重签链接  4)删除")
	switch ask("  选择: ") {
	case "1":
		run(cmdFile([]string{"list"}))
	case "2":
		p := ask("  本地路径: ")
		if p == "" {
			return
		}
		args := []string{"put", p}
		if e := ask("  有效小时(可空默认6): "); e != "" {
			args = append(args, "--expires", e)
		}
		run(cmdFile(args))
	case "3":
		if n := ask("  文件名: "); n != "" {
			run(cmdFile([]string{"link", n}))
		}
	case "4":
		if n := ask("  文件名: "); n != "" {
			run(cmdFile([]string{"rm", n}))
		}
	}
}
