package powerguard

import (
	"context"
	"fmt"
	"time"
)

// backup_runner.go 负责备份任务的异步运行与状态维护。
//
// 一次只允许一个任务在跑：备份通常是磁盘密集型操作，并发跑多份只会互相拖慢，
// 而且按键连按也不该叠起一堆 rsync。第二次触发会得到一个明确提示。

type backupRunState struct {
	running   bool
	jobID     string
	jobName   string
	startedAt time.Time
	last      *BackupResult
	// lastAuto 是自动备份（多块打标卷）的汇总结果；普通任务运行时会清掉。
	lastAuto  *AutoBackupResult
	lastError string
}

// StartBackupJob 在后台启动一次备份，立即返回。真正的执行结果通过
// BackupStatus 查询，并在结束时用蜂鸣 + 闪灯给出可听见/看得见的回执。
func (m *Manager) StartBackupJob(job GPIOBackupJob, dryRun bool) error {
	m.backupMu.Lock()
	if m.backupRun.running {
		name := m.backupRun.jobName
		m.backupMu.Unlock()
		return fmt.Errorf("已有备份任务正在运行（%s），请等它结束", name)
	}
	m.backupRun.running = true
	m.backupRun.jobID = job.ID
	m.backupRun.jobName = job.Name
	m.backupRun.startedAt = time.Now()
	m.backupMu.Unlock()

	go func() {
		result, err := m.RunBackupJob(context.Background(), job, dryRun)
		m.finishBackupRun(job.Name, &result, nil, err, dryRun)
	}()
	return nil
}

// RunBackupJobNow 同步执行一次备份，供界面上的「立即运行」按钮做即时反馈。
func (m *Manager) RunBackupJobNow(ctx context.Context, job GPIOBackupJob, dryRun bool) (BackupResult, error) {
	m.backupMu.Lock()
	if m.backupRun.running {
		name := m.backupRun.jobName
		m.backupMu.Unlock()
		return BackupResult{}, fmt.Errorf("已有备份任务正在运行（%s），请等它结束", name)
	}
	m.backupRun.running = true
	m.backupRun.jobID = job.ID
	m.backupRun.jobName = job.Name
	m.backupRun.startedAt = time.Now()
	m.backupMu.Unlock()

	result, err := m.RunBackupJob(ctx, job, dryRun)
	m.finishBackupRun(job.Name, &result, nil, err, dryRun)
	return result, err
}

// StartAutoBackupJob 在后台启动一次自动备份（可能覆盖多块打标卷），立即返回。
func (m *Manager) StartAutoBackupJob(rule GPIOAutoBackup, dryRun bool) error {
	m.backupMu.Lock()
	if m.backupRun.running {
		name := m.backupRun.jobName
		m.backupMu.Unlock()
		return fmt.Errorf("已有备份任务正在运行（%s），请等它结束", name)
	}
	m.backupRun.running = true
	m.backupRun.jobID = rule.ID
	m.backupRun.jobName = "自动备份：" + rule.Name
	m.backupRun.startedAt = time.Now()
	m.backupMu.Unlock()

	go func() {
		result, err := m.RunAutoBackupJob(context.Background(), rule, dryRun)
		m.finishBackupRun(rule.Name, nil, &result, err, dryRun)
	}()
	return nil
}

// RunAutoBackupJobNow 同步执行一次自动备份，供界面「立即运行」做即时反馈。
func (m *Manager) RunAutoBackupJobNow(ctx context.Context, rule GPIOAutoBackup, dryRun bool) (AutoBackupResult, error) {
	m.backupMu.Lock()
	if m.backupRun.running {
		name := m.backupRun.jobName
		m.backupMu.Unlock()
		return AutoBackupResult{}, fmt.Errorf("已有备份任务正在运行（%s），请等它结束", name)
	}
	m.backupRun.running = true
	m.backupRun.jobID = rule.ID
	m.backupRun.jobName = "自动备份：" + rule.Name
	m.backupRun.startedAt = time.Now()
	m.backupMu.Unlock()

	result, err := m.RunAutoBackupJob(ctx, rule, dryRun)
	m.finishBackupRun(rule.Name, nil, &result, err, dryRun)
	return result, err
}

// finishBackupRun 一次备份跑完后的统一收尾。试跑走单独一条路：只把运行标记放下，
// 既不写 last / lastAuto，也不播放蜂鸣闪灯——试跑没有真的写过任何文件，不该让
// 机箱响、更不该把界面上的「上次备份完成」顶掉。
func (m *Manager) finishBackupRun(jobName string, result *BackupResult, auto *AutoBackupResult, err error, dryRun bool) {
	if dryRun {
		m.backupMu.Lock()
		m.backupRun.running = false
		m.backupMu.Unlock()
		return
	}
	m.recordBackupFinish(jobName, result, auto, err, "copy", "复制按键")
}

// feedbackBackupResult 已并入 recordBackupFinish：普通任务与自动备份共用同一套
// 回执（成功闪 3 下 + 滴滴滴 3 声，失败一声长鸣）。

// BackupStatus 返回当前备份运行状态与最近一次结果。
func (m *Manager) BackupStatus() BackupStatus {
	m.backupMu.Lock()
	defer m.backupMu.Unlock()
	status := BackupStatus{
		Running:   m.backupRun.running,
		JobID:     m.backupRun.jobID,
		JobName:   m.backupRun.jobName,
		StartedAt: "",
		LastError: m.backupRun.lastError,
	}
	if !m.backupRun.startedAt.IsZero() {
		status.StartedAt = m.backupRun.startedAt.Format(time.RFC3339)
	}
	if m.backupRun.last != nil {
		clone := *m.backupRun.last
		status.Last = &clone
	}
	if m.backupRun.lastAuto != nil {
		clone := *m.backupRun.lastAuto
		status.Auto = &clone
	}
	return status
}

// BackupJobByID 按 ID 取任务配置（界面「立即运行」用）。
func (m *Manager) BackupJobByID(id string) (GPIOBackupJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		return GPIOBackupJob{}, false
	}
	for _, job := range cfg.GPIO.Backups {
		if job.ID == id {
			return job, true
		}
	}
	return GPIOBackupJob{}, false
}

// AutoBackupRuleByID 按 ID 取自动备份规则（按键 / 界面「立即运行」用）。
func (m *Manager) AutoBackupRuleByID(id string) (GPIOAutoBackup, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		return GPIOAutoBackup{}, false
	}
	for _, rule := range cfg.GPIO.Autos {
		if rule.ID == id {
			return rule, true
		}
	}
	return GPIOAutoBackup{}, false
}

// recordBackupFinish 统一收尾：写运行状态并给出蜂鸣 + 闪灯回执。
func (m *Manager) recordBackupFinish(jobName string, result *BackupResult, auto *AutoBackupResult, err error, buttonID, buttonName string) {
	m.backupMu.Lock()
	m.backupRun.running = false
	if err != nil {
		m.backupRun.lastError = err.Error()
		m.backupRun.last = nil
		// 自动备份失败时保留汇总（处理了几块盘、每块的结果），界面上
		// 才能指出问题出在哪块盘；没有实际处理过卷（如没找到打标卷）则清空。
		if auto != nil && (auto.Volumes > 0 || len(auto.Results) > 0) {
			clone := *auto
			m.backupRun.lastAuto = &clone
		} else {
			m.backupRun.lastAuto = nil
		}
	} else if auto != nil {
		m.backupRun.lastError = ""
		m.backupRun.last = nil
		clone := *auto
		m.backupRun.lastAuto = &clone
	} else if result != nil {
		m.backupRun.lastError = ""
		m.backupRun.lastAuto = nil
		clone := *result
		m.backupRun.last = &clone
	}
	m.backupMu.Unlock()

	pattern := gpioStandardFeedback()
	stage := "备份完成"
	if err != nil {
		pattern = GPIOFeedbackPattern{Tones: 1}
		stage = "备份失败"
	}
	_ = m.PlayGPIOFeedback(GPIOEvent{
		ButtonID: buttonID, Name: buttonName, Stage: stage,
		Kind: GPIOEventFeedback, Feedback: &pattern,
	})
}
