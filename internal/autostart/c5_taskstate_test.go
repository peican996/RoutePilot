package autostart

import (
	"errors"
	"testing"
)

// schtasks 的退出码无法区分"任务不存在"与"权限不足"，
// 必须按输出归类：前者允许安全地创建另一种自启，后者绝不能。
func TestClassifyTaskQuery(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		err     error
		want    taskState
		wantErr bool
	}{
		{"present", "", nil, taskPresent, false},
		{"not-found-en", "ERROR: The system cannot find the file specified.", errors.New("exit status 1"), taskNotFound, false},
		{"not-found-cn", "错误: 系统找不到指定的文件。", errors.New("exit status 1"), taskNotFound, false},
		{"not-found-alt", "Task does not exist.", errors.New("exit status 1"), taskNotFound, false},
		{"access-denied", "ERROR: Access is denied.", errors.New("exit status 1"), taskUnknown, true},
		{"unexpected", "", errors.New("exec: not found"), taskUnknown, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyTaskQuery([]byte(tc.out), tc.err)
			if got != tc.want {
				t.Fatalf("状态分类错误：want %v, got %v", tc.want, got)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("错误返回不符：wantErr=%v, err=%v", tc.wantErr, err)
			}
		})
	}
}

func withAutostartHooks(t *testing.T, q func() (taskState, error), onWrite func() error, onDelete func()) {
	t.Helper()
	origQuery, origWrite, origDelete := queryTask, writeRunValue, deleteRunValue
	t.Cleanup(func() {
		queryTask, writeRunValue, deleteRunValue = origQuery, origWrite, origDelete
	})
	queryTask = q
	writeRunValue = onWrite
	deleteRunValue = onDelete
}

// 计划任务查询权限不足（状态未知）时开启自启：必须报错且不写 Run 项，
// 否则会与真实存在的计划任务并存，登录时双实例启动。
func TestSetUnknownTaskStateNeverWritesRunValue(t *testing.T) {
	var writes, deletes int
	withAutostartHooks(t,
		func() (taskState, error) { return taskUnknown, errors.New("access denied") },
		func() error { writes++; return nil },
		func() { deletes++ },
	)

	if err := Set(true, false); err == nil {
		t.Fatal("任务状态未知时开启普通自启必须报错")
	}
	if err := Set(true, true); err == nil {
		t.Fatal("任务状态未知时开启管理员自启必须报错")
	}
	if writes != 0 {
		t.Fatalf("状态未知时绝不能写入 Run 项，实际写入 %d 次", writes)
	}
}

// 计划任务已存在时开启管理员自启：应清理 Run 项，避免双实例。
func TestSetWithExistingTaskConvergesRunValue(t *testing.T) {
	var writes, deletes int
	withAutostartHooks(t,
		func() (taskState, error) { return taskPresent, nil },
		func() error { writes++; return nil },
		func() { deletes++ },
	)

	if err := Set(true, true); err != nil {
		t.Fatalf("计划任务已存在时开启应成功: %v", err)
	}
	if writes != 0 {
		t.Fatalf("计划任务已存在时不应再写 Run 项，实际 %d 次", writes)
	}
	if deletes == 0 {
		t.Fatal("计划任务已存在时必须清理 Run 项，否则会双实例")
	}
}

// 非管理员开启自启，但计划任务已存在：必须报错引导提权，不能静默写 Run 项。
func TestSetNonElevatedWithExistingTaskRejected(t *testing.T) {
	var writes int
	withAutostartHooks(t,
		func() (taskState, error) { return taskPresent, nil },
		func() error { writes++; return nil },
		func() {},
	)

	if err := Set(true, false); err == nil {
		t.Fatal("计划任务存在时非提权开启自启必须报错")
	}
	if writes != 0 {
		t.Fatal("不得写入 Run 项")
	}
}

// 管理员升级自启时任务状态未知：必须报错且不改动 Run 项。
func TestUpgradeIfElevatedUnknownStateIsNoop(t *testing.T) {
	var deletes int
	withAutostartHooks(t,
		func() (taskState, error) { return taskUnknown, errors.New("access denied") },
		func() error { return nil },
		func() { deletes++ },
	)

	if err := UpgradeIfElevated(); err == nil {
		t.Fatal("任务状态未知时升级必须报错")
	}
	if deletes != 0 {
		t.Fatal("任务状态未知时不得改动 Run 项")
	}
}
