package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
)

// Client 结构体，用于与判题沙盒交互
type Client struct {
	httpClient *http.Client
}

// NewClient 创建一个新的判题沙盒客户端
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 30 * time.Second, // 设置请求超时
		},
	}
}

// Judge 向指定的沙盒发送判题请求
func (c *Client) Judge(sandboxAddr string, task *model.Task) (*model.JudgeResult, error) {
	fmt.Printf("Sending judge request for task %s to sandbox %s...\n", task.ID, sandboxAddr)

	// 准备请求体
	requestBody, err := json.Marshal(task) // 假设沙盒接收 Task 对象
	if err != nil {
		return nil, fmt.Errorf("failed to marshal judge request: %w", err)
	}

	// 构建 HTTP 请求
	url := fmt.Sprintf("http://%s/judge", sandboxAddr) // 假设沙盒的判题接口是 /judge
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(requestBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create judge request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 发送请求
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send judge request to %s: %w", sandboxAddr, err)
	}
	defer resp.Body.Close()

	// 处理响应
	if resp.StatusCode != http.StatusOK {
		// 可以读取响应体获取更详细的错误信息
		return nil, fmt.Errorf("sandbox %s returned non-OK status: %d", sandboxAddr, resp.StatusCode)
	}

	var result model.JudgeResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode judge response from %s: %w", sandboxAddr, err)
	}

	fmt.Printf("Received judge result for task %s from sandbox %s.\n", task.ID, sandboxAddr)
	return &result, nil
}
