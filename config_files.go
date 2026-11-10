package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// currentConfigVersion 是本程序写出的配置格式版本。
// 读取到更高版本说明用户降级了程序：不能按旧结构解释，更不能覆盖。
const currentConfigVersion = 1

// configReadState 区分配置文件的各种异常情况，避免把损坏当成空配置。
type configReadState string

const (
	configMissing     configReadState = "missing"     // 文件不存在，属于正常首次启动
	configEmpty       configReadState = "empty"       // 文件存在但内容为空/只有空白（通常是截断）
	configCorrupt     configReadState = "corrupt"     // JSON 解析失败
	configUnsupported configReadState = "unsupported" // 版本高于当前程序
	configOK          configReadState = "ok"
)

// readConfigJSON 读取并解析一个 JSON 配置文件。
//
// 与直接 json.Unmarshal 的区别：明确区分"不存在"（可以安全地当作首次启动）
// 与"空文件/损坏"（绝不能静默当作空配置继续，否则下一次保存会覆盖用户数据）。
func readConfigJSON(path string, dst any) (configReadState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return configMissing, nil
		}
		return configCorrupt, err
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	if len(bytes.TrimSpace(b)) == 0 {
		return configEmpty, nil
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return configCorrupt, err
	}
	return configOK, nil
}

// preserveCorruptFile 把损坏的配置改名保留，返回备份路径。
// 改名而不是复制：原路径必须腾空，否则后续合法保存会把损坏内容覆盖掉，
// 用户就再也拿不回原始数据。改名失败时退化为复制，并保留原文件。
func preserveCorruptFile(path string) (string, error) {
	backup := fmt.Sprintf("%s.corrupt-%s", path, time.Now().Format("20060102-150405"))
	if err := os.Rename(path, backup); err == nil {
		return backup, nil
	}
	// 改名失败，退化为复制并保留原文件。复制成功也算备份成功。
	data, readErr := os.ReadFile(path)
	if readErr == nil {
		if werr := os.WriteFile(backup, data, 0o600); werr != nil {
			return "", fmt.Errorf("备份损坏文件失败: %w", werr)
		}
		return backup, nil
	}
	// rename 与 read 双双失败：两级错误都要保留，
	// 否则用户只看到一句没有原因的"无法保留"，损坏无从排查。
	return "", fmt.Errorf("无法保留损坏文件: %w（改名失败，读取也失败: %v）", readErr, filepath.Base(path))
}

// reportConfigProblem 统一记录配置异常：写 debug.log 并在界面启动错误里提示。
// 必须让用户知道"配置没被读取"，而不是以为自己的规则/订阅凭空消失了。
func (s *CoreService) reportConfigProblem(path string, state configReadState, cause error) {
	name := filepath.Base(path)
	var desc string
	switch state {
	case configEmpty:
		desc = "内容为空或已被截断"
	case configCorrupt:
		desc = "格式损坏"
	case configUnsupported:
		desc = fmt.Sprintf("版本高于当前程序（支持 %d）", currentConfigVersion)
	default:
		return
	}

	backup, berr := preserveCorruptFile(path)
	msg := fmt.Sprintf("配置文件 %s %s，已停止使用", name, desc)
	if backup != "" {
		msg += "，原文件已备份为 " + filepath.Base(backup)
	}
	if cause != nil {
		msg += "（" + cause.Error() + "）"
	}
	if berr != nil {
		msg += "；" + berr.Error()
	}
	s.dbg("%s", msg)
	s.appendBootErr(msg)
}

// configVersionUnsupported 判断一个已解析配置的版本是否超出当前程序能力。
func configVersionUnsupported(version int) bool {
	return version > currentConfigVersion
}
