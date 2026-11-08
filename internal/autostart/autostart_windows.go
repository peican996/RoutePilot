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
	"unicode/utf16"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey  = `Software\Microsoft\Windows\CurrentVersion\Run`
	appName = "NexusLink"
	// 旧版本的启动项名。产品改名后要顺手清掉，否则注册表里
	// 会留一条指向新路径但叫 ProxDeck 的僵尸启动项。
	legacyAppName = "ProxDeck"

	// 管理员自启的计划任务名。用计划任务而不是 Run 键，才能带
	// HighestAvailable 权限静默启动，TUN 模式开机后不用再手动提权。
	taskName = "NexusLink"
)

// Enabled 检查是否已经配置了开机启动（普通 Run 项或管理员计划任务）。
func Enabled() bool {
	return taskExists() || runValueExists()
}

// Set 配置开机启动。
//
// elevated=false 时只能写 HKCU Run：启动后是普通权限，TUN 仍需要手动提权。
// elevated=true 时创建 HighestAvailable 的计划任务，开机静默以管理员运行；
// 创建失败会回退到 Run 项并返回错误，至少保留普通自启。
func Set(enable bool, elevated bool) error {
	if !enable {
		removeRunValue()
		if taskExists() {
			if err := deleteTask(); err != nil {
				return fmt.Errorf("关闭管理员自启失败（需要管理员权限）: %w", err)
			}
		}
		return nil
	}

	if elevated {
		if taskExists() {
			removeRunValue()
			return nil
		}
		if err := createTask(); err != nil {
			fallback := setRunValue()
			if fallback != nil {
				return fmt.Errorf("创建管理员自启失败，普通自启也失败: %v / %w", err, fallback)
			}
			return fmt.Errorf("创建管理员自启失败，已回退为普通自启: %w", err)
		}
		removeRunValue()
		return nil
	}

	// 非管理员：保持原有免 UAC 的 Run 自启。
	return setRunValue()
}

// UpgradeIfElevated 在管理员进程里把已有的 Run 自启升级为计划任务。
// 只应在确认当前进程已提权时调用；失败时保留原 Run 项。
func UpgradeIfElevated() error {
	if taskExists() || !runValueExists() {
		return nil
	}
	if err := createTask(); err != nil {
		return err
	}
	removeRunValue()
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
	v, _, err := k.GetStringValue(legacyAppName)
	return err == nil && v != ""
}

func setRunValue() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	_ = k.DeleteValue(legacyAppName)

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
	_ = k.DeleteValue(legacyAppName)
}

func taskExists() bool {
	cmd := exec.Command("schtasks", "/Query", "/TN", taskName)
	return cmd.Run() == nil
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

	f, err := os.CreateTemp("", "nexuslink-task-*.xml")
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
    <Description>NexusLink 开机自启（管理员）</Description>
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
