package scheduler

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/discovery"
)

// Scheduler 结构体，负责选择判题沙盒
type Scheduler struct {
	discoveryClient *discovery.ZookeeperClient
	sandboxes       []string // 缓存的沙盒列表
	mu              sync.RWMutex
	rand            *rand.Rand // 用于随机选择
}

// NewScheduler 创建一个新的调度器
func NewScheduler(discoveryClient *discovery.ZookeeperClient) *Scheduler {
	fmt.Println("Initializing Scheduler...")
	scheduler := &Scheduler{
		discoveryClient: discoveryClient,
		rand:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	// 启动后台 goroutine 监听沙盒变化
	go scheduler.watchSandboxes("/sandboxes") // ZK 路径示例
	return scheduler
}

// watchSandboxes 监听 Zookeeper 中沙盒列表的变化
func (s *Scheduler) watchSandboxes(path string) {
	// 首次获取
	initialSandboxes, err := s.discoveryClient.GetSandboxes(path)
	if err != nil {
		fmt.Printf("Scheduler: Failed to get initial sandboxes: %v\n", err)
		// 可能需要重试或记录错误
	} else {
		s.updateSandboxes(initialSandboxes)
	}

	// 开始监听
	events, err := s.discoveryClient.WatchSandboxes(path)
	if err != nil {
		fmt.Printf("Scheduler: Failed to watch sandboxes: %v\n", err)
		// 可能需要重试或记录错误
		return
	}

	for sandboxList := range events {
		s.updateSandboxes(sandboxList)
	}
	fmt.Println("Scheduler: Watch channel closed.")
}

// updateSandboxes 更新缓存的沙盒列表
func (s *Scheduler) updateSandboxes(newSandboxes []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sandboxes = newSandboxes
	fmt.Printf("Scheduler: Updated sandbox list: %v\n", s.sandboxes)
}

// SelectSandbox 选择一个最优的判题沙盒
// 这里先实现一个简单的随机选择策略
func (s *Scheduler) SelectSandbox() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.sandboxes) == 0 {
		return "", fmt.Errorf("no available sandboxes")
	}

	// 随机选择
	index := s.rand.Intn(len(s.sandboxes))
	selected := s.sandboxes[index]
	fmt.Printf("Scheduler: Selected sandbox: %s\n", selected)
	return selected, nil

	// TODO: 实现更复杂的调度算法，例如：
	// - 轮询 (Round Robin)
	// - 最少连接 (Least Connections)
	// - 基于负载 (Load-based, 需要沙盒上报负载信息)
	// - 一致性哈希 (Consistent Hashing)
}
