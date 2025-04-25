package discovery

import (
	"fmt"
	"time"

	"github.com/samuel/go-zookeeper/zk"
)

// ZookeeperClient 结构体，封装 Zookeeper 客户端
type ZookeeperClient struct {
	conn *zk.Conn
}

// NewZookeeperClient 创建一个新的 Zookeeper 客户端连接
func NewZookeeperClient(servers []string) (*ZookeeperClient, error) {
	fmt.Println("Connecting to Zookeeper...")
	conn, _, err := zk.Connect(servers, time.Second*5) // 5 秒超时
	if err != nil {
		return nil, fmt.Errorf("failed to connect to zookeeper: %w", err)
	}
	return &ZookeeperClient{conn: conn}, nil
	// return &ZookeeperClient{}, nil // 临时返回
}

// GetSandboxes 获取可用的判题沙盒列表
func (zc *ZookeeperClient) GetSandboxes(path string) ([]string, error) {
	fmt.Printf("Getting sandboxes from Zookeeper path: %s\n", path)
	children, _, err := zc.conn.Children(path)
	if err != nil {
		return nil, fmt.Errorf("failed to get children for path %s: %w", path, err)
	}
	return children, nil
	// 临时返回模拟数据
	// return []string{"sandbox1.example.com:8080", "sandbox2.example.com:8080"}, nil
}

// WatchSandboxes 监听判题沙盒节点变化
func (zc *ZookeeperClient) WatchSandboxes(path string) (<-chan []string, error) {
	fmt.Printf("Watching Zookeeper path: %s\n", path)
	events := make(chan []string)
	go func() {
		defer close(events)
		for {
			children, _, ch, err := zc.conn.ChildrenW(path)
			if err != nil {
				fmt.Printf("Error watching children for path %s: %v\n", path, err)
				// 可以考虑添加重试逻辑或退出 goroutine
				time.Sleep(5 * time.Second)
				continue
			}
			events <- children // 发送当前列表
			<-ch               // 等待 Zookeeper 事件
		}
	}()
	return events, nil
	/* // 临时返回
	events := make(chan []string, 1) // 临时返回
	events <- []string{"sandbox1.example.com:8080", "sandbox2.example.com:8080"}
	return events, nil
	*/
}

// Close 关闭 Zookeeper 连接
func (zc *ZookeeperClient) Close() {
	fmt.Println("Closing Zookeeper connection...")
	zc.conn.Close()
}
