//go:build windows

package autostart

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"html"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey  = `Software\Microsoft\Windows\CurrentVersion\Run`
	appName = "RoutePilot"

	// 管理员自启的计划任务名。用计划任务而不是 Run 键，才能带
	// HighestAvailable 权限静默启动，TUN 模式开机后不用再手动提权。
	taskName = "RoutePilot"
)

// legacyAppNames 是旧版本的启动项名（产品改过两次名）。要顺手清掉，
// 否则注册表里会留一条指向新路径但叫旧名字的僵尸启动项。
var legacyAppNames = []string{"NexusLink", "ProxDeck"}

// taskState 是计划任务查询的三态结果。
// 查询失败（权限不足、schtasks 异常）绝不能等同于"不存在"：
// 否则任务真实存在时会再写一份 Run 项，导致登录时双实例启动。
type taskState int

const (
	taskUnknown taskState = iota
	taskNotFound
	taskPresent
)

// 以下三个是测试注入点。生产环境分别指向真实实现；
// 测试用它们模拟"计划任务查询权限不足"等故障，且不触碰真实注册表。
var (
	queryTask      = queryTaskState
	writeRunValue  = setRunValue
	deleteRunValue = removeRunValue
)

// classifyTaskQuery 把 schtasks 的查询结果归类为三态。
// 非零退出既可能是"任务不存在"，也可能是权限不足，必须看输出区分。
func classifyTaskQuery(out []byte, err error) (taskState, error) {
	if err == nil {
		return taskPresent, nil
	}
	msg := strings.ToLower(string(out))
	switch {
	case strings.Contains(msg, "cannot find"),
		strings.Contains(msg, "找不到"),
		strings.Contains(msg, "does not exist"),
		strings.Contains(msg, "not exist"):
		return taskNotFound, nil
	default:
		return taskUnknown, fmt.Errorf("查询计划任务 %s 失败: %v（%s）", taskName, err, strings.TrimSpace(string(out)))
	}
}

// queryTaskState 查询管理员自启计划任务的状态。
func queryTaskState() (taskState, error) {
	cmd := exec.Command("schtasks", "/Query", "/TN", taskName)
	out, err := cmd.CombinedOutput()
	return classifyTaskQuery(out, err)
}

// Enabled 检查是否已经配置了开机启动（普通 Run 项或管理员计划任务）。
func Enabled() bool {
	if runValueExists() {
		return true
	}
	st, err := queryTask()
	if err != nil {
		// 无法确认时按"可能已启用"处理，避免界面谎报关闭。
		return true
	}
	return st == taskPresent
}

// Set 配置开机启动。
//
// elevated=false 时只能写 HKCU Run：启动后是普通权限，TUN 仍需要手动提权。
// elevated=true 时创建 HighestAvailable 的计划任务，开机静默以管理员运行；
// 创建失败会回退到 Run 项并返回错误，至少保留普通自启。
func Set(enable bool, elevated bool) error {
	st, qerr := queryTask()
	if qerr != nil {
		// 状态未知时不做任何会新增启动项的操作，宁可报错也不能造成双实例。
		if !enable {
			deleteRunValue()
		}
		return qerr
	}

	if !enable {
		deleteRunValue()
		if st == taskPresent {
			if err := deleteTask(); err != nil {
				return fmt.Errorf("关闭管理员自启失败（需要管理员权限）: %w", err)
			}
		}
		return nil
	}

	if elevated {
		if st == taskPresent {
			deleteRunValue()
			return nil
		}
		if err := createTask(); err != nil {
			fallback := writeRunValue()
			if fallback != nil {
				return fmt.Errorf("创建管理员自启失败，普通自启也失败: %v / %w", err, fallback)
			}
			return fmt.Errorf("创建管理员自启失败，已回退为普通自启: %w", err)
		}
		deleteRunValue()
		return nil
	}

	// 非管理员：保持原有免 UAC 的 Run 自启。但如果此前提权时建过
	// 计划任务，两者并存会导致登录时双实例、每次开机弹"已经在运行"。
	// 计划任务只有管理员能删，这里如实报错引导用户走提权路径，
	// 绝不能静默再写一份 Run 项让坏状态固化。
	if st == taskPresent {
		return fmt.Errorf("检测到管理员自启的计划任务：请以管理员身份运行本程序后再切换自启方式，否则会双实例启动")
	}
	return writeRunValue()
}

// UpgradeIfElevated 在管理员进程里把已有的 Run 自启升级为计划任务。
// 只应在确认当前进程已提权时调用；失败时保留原 Run 项。
func UpgradeIfElevated() error {
	st, err := queryTask()
	if err != nil {
		return err
	}
	if st == taskNotFound && runValueExists() {
		if err := createTask(); err != nil {
			return err
		}
	}
	// 无论任务是刚建的还是早已存在，都要收敛掉 Run 项：
	// 两者并存会双实例启动，这个错误状态不能自我延续。
	deleteRunValue()
	return nil
}

func runValueExists() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	if v, _, err := k.GetStringValue(appName); err == nil && v != "" {
		return true
	}
	// 旧名字的启动项也算"开着"，否则升级后用户会看到开关被莫名关掉
	for _, legacy := range legacyAppNames {
		if v, _, err := k.GetStringValue(legacy); err == nil && v != "" {
			return true
		}
	}
	return false
}

func setRunValue() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	// 清理旧名字的启动项，避免两个同名程序同时自启
	for _, legacy := range legacyAppNames {
		_ = k.DeleteValue(legacy)
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// 路径可能含空格，必须加引号，否则 Windows 会截断。
	return k.SetStringValue(appName, `"`+exe+`"`)
}

func removeRunValue() {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.DeleteValue(appName)
	for _, legacy := range legacyAppNames {
		_ = k.DeleteValue(legacy)
	}
}

func createTask() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	xmlData, err := taskXML(exe)
	if err != nil {
		return err
	}

	f, err := os.CreateTemp("", "routepilot-task-*.xml")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(xmlData); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	out, err := exec.Command("schtasks", "/Create", "/TN", taskName, "/XML", name, "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func deleteTask() error {
	out, err := exec.Command("schtasks", "/Delete", "/TN", taskName, "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// taskXML 生成计划任务定义。用 XML 而不是 schtasks /TR 参数，
// 避免安装路径带空格时的命令行引号问题。
// schtasks 要求 XML 是 UTF-16 LE（带 BOM）。
func taskXML(exe string) ([]byte, error) {
	return taskXMLWithRunLevel(exe, "HighestAvailable")
}

func taskXMLWithRunLevel(exe, runLevel string) ([]byte, error) {
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	userID := html.EscapeString(u.Username)
	command := html.EscapeString(exe)

	text := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>RoutePilot 开机自启（管理员）</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>%s</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>%s</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
    </Exec>
  </Actions>
</Task>`, userID, userID, runLevel, command)

	var buf bytes.Buffer
	// UTF-16 LE BOM
	buf.Write([]byte{0xFF, 0xFE})
	for _, r := range utf16.Encode([]rune(text)) {
		if err := binary.Write(&buf, binary.LittleEndian, r); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}
