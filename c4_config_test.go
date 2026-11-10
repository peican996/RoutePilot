package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 损坏配置必须：保留原文件（改名备份）、记录可诊断错误、不得静默当作空配置。
func TestCorruptConfigIsPreservedAndReported(t *testing.T) {
	cases := []struct {
		name   string
		file   string
		body   string
		load   func(*CoreService)
		verify func(*testing.T, *CoreService)
	}{
		{
			name: "app-state 非法 JSON",
			file: appStateFile,
			body: `{"tun": true, "mode": `,
			load: (*CoreService).loadAppState,
			verify: func(t *testing.T, s *CoreService) {
				s.mu.RLock()
				mode := s.mode
				s.mu.RUnlock()
				if mode != "rule" {
					t.Fatalf("损坏后应回退 rule 模式，实际 %q", mode)
				}
			},
		},
		{
			name: "app-state 字段类型非法",
			file: appStateFile,
			body: `{"tun": "yes"}`,
			load: (*CoreService).loadAppState,
		},
		{
			name: "app-state 截断",
			file: appStateFile,
			body: `{"tun": true, "proxyOn":`,
			load: (*CoreService).loadAppState,
		},
		{
			name: "subs 损坏",
			file: subsFile,
			body: `{"items": [{"url": `,
			load: (*CoreService).loadSubs,
			verify: func(t *testing.T, s *CoreService) {
				s.mu.RLock()
				n := len(s.subs)
				s.mu.RUnlock()
				if n != 0 {
					t.Fatalf("损坏订阅不应被加载: %d", n)
				}
			},
		},
		{
			name: "proc-rules 损坏",
			file: procRulesJSON,
			body: `{"name:chrome.exe": `,
			load: (*CoreService).loadProcRules,
		},
		{
			name: "site-rules 损坏",
			file: siteRulesJSON,
			body: `[{"domain": "example.com",`,
			load: (*CoreService).loadSiteRules,
		},
		{
			name: "ignored 损坏",
			file: procIgnoredJSON,
			body: `["chrome.exe"`,
			load: (*CoreService).loadIgnored,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := configTestService(t)
			path := filepath.Join(dataDir(), tc.file)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}

			tc.load(svc)

			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("损坏文件必须被移走，避免后续保存覆盖原始内容")
			}
			backups, _ := filepath.Glob(path + ".corrupt-*")
			if len(backups) != 1 {
				t.Fatalf("应生成一个损坏备份，实际 %d", len(backups))
			}
			data, err := os.ReadFile(backups[0])
			if err != nil || string(data) != tc.body {
				t.Fatalf("备份内容与原文件不一致: %q", data)
			}
			svc.mu.RLock()
			bootErr := svc.bootErr
			svc.mu.RUnlock()
			if !strings.Contains(bootErr, filepath.Base(path)) {
				t.Fatalf("启动错误应指明损坏文件，实际 %q", bootErr)
			}
			if tc.verify != nil {
				tc.verify(t, svc)
			}
		})
	}
}

// 空文件按截断处理，同样保留并提示。
func TestEmptyConfigIsTreatedAsCorrupt(t *testing.T) {
	svc := configTestService(t)
	path := filepath.Join(dataDir(), appStateFile)
	if err := os.WriteFile(path, []byte("   \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.loadAppState()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("空配置文件必须被保留为备份")
	}
	if backups, _ := filepath.Glob(path + ".corrupt-*"); len(backups) != 1 {
		t.Fatal("空配置文件应生成备份")
	}
	svc.mu.RLock()
	bootErr := svc.bootErr
	svc.mu.RUnlock()
	if !strings.Contains(bootErr, "内容为空") {
		t.Fatalf("错误信息应区分空文件，实际 %q", bootErr)
	}
}

// 版本高于当前程序：不得按旧结构解释，也不得覆盖。
func TestFutureVersionConfigIsRejected(t *testing.T) {
	svc := configTestService(t)
	path := filepath.Join(dataDir(), appStateFile)
	if err := os.WriteFile(path, []byte(`{"version": 99, "tun": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.loadAppState()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("未来版本配置必须被移走")
	}
	svc.mu.RLock()
	bootErr := svc.bootErr
	tun := svc.tunOn
	svc.mu.RUnlock()
	if !strings.Contains(bootErr, "版本") {
		t.Fatalf("应提示版本不兼容，实际 %q", bootErr)
	}
	if tun {
		t.Fatal("不能按未来版本配置开启 TUN")
	}
}

// 文件不存在属于正常首次启动：不提示、不产生备份。
func TestMissingConfigIsNormal(t *testing.T) {
	svc := configTestService(t)
	svc.loadAppState()
	svc.loadSubs()
	svc.loadProcRules()
	svc.loadSiteRules()
	svc.loadIgnored()

	svc.mu.RLock()
	bootErr := svc.bootErr
	svc.mu.RUnlock()
	if bootErr != "" {
		t.Fatalf("首次启动不应有配置错误: %q", bootErr)
	}
	entries, _ := filepath.Glob(filepath.Join(dataDir(), "*.corrupt-*"))
	if len(entries) != 0 {
		t.Fatalf("首次启动不应产生损坏备份: %v", entries)
	}
}

// 损坏文件被保留后，后续保存只能写入新文件，不能破坏备份。
func TestSaveAfterCorruptDoesNotDestroyBackup(t *testing.T) {
	svc := configTestService(t)
	path := filepath.Join(dataDir(), appStateFile)
	bad := `{"tun": true,`
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.loadAppState()
	backups, _ := filepath.Glob(path + ".corrupt-*")
	if len(backups) != 1 {
		t.Fatal("应生成损坏备份")
	}

	svc.saveAppState()

	got, err := os.ReadFile(backups[0])
	if err != nil || string(got) != bad {
		t.Fatalf("备份被后续保存破坏: %q", got)
	}
	fresh, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("保存应生成新的配置文件")
	}
	if !strings.Contains(string(fresh), `"version": 1`) {
		t.Fatalf("新配置应带当前版本号: %s", fresh)
	}
}

func configTestService(t *testing.T) *CoreService {
	t.Helper()
	old := getDataDirOverride()
	setDataDirOverride(t.TempDir())
	t.Cleanup(func() { setDataDirOverride(old) })
	return NewCoreService()
}
