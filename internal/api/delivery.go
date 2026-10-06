package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"time"
)

// 事件投递（outbox 事件投递，落地方案 §5）：投递 worker 定时拉取待投递行，
// 按订阅通道投递（webhook：URL POST 事件 JSON——兼容 Slack/Feishu incoming
// webhook 形态；email：SMTP），成功删行、失败指数退避（store.OutboxRetry）。

// Deliverer 是投递循环（api 进程内，与 Aggregator 同构）。
type Deliverer struct {
	Store        Store
	HTTP         *http.Client
	SMTP         SMTPConfig // 零值 = email 通道禁用
	Logger       Logger
	Batch        int
	AllowPrivate bool // SSRF 防护放行开关（本地/e2e；生产默认拒绝）
}

type Logger interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}

// SMTPConfig 是邮件通道配置（SMTP_HOST/PORT/FROM 环境变量）。
type SMTPConfig struct {
	Host string
	Port string
	From string
}

// Run 周期投递（interval 默认 5s）。
func (d *Deliverer) Run(ctx context.Context, interval time.Duration) {
	if d.HTTP == nil {
		d.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.pass(ctx); err != nil && d.Logger != nil {
				d.Logger.Warn("deliver pass failed", "err", err)
			}
		}
	}
}

func (d *Deliverer) pass(ctx context.Context) error {
	if d.HTTP == nil {
		d.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	rows, err := d.Store.ListPendingOutbox(ctx, d.Batch)
	if err != nil {
		return err
	}
	for _, row := range rows {
		typ, payload, at, err := d.Store.GetEvent(ctx, row.EventID)
		if err != nil {
			// 事件行缺失（归档等）：不再重试
			_ = d.Store.OutboxDelivered(ctx, row.ID)
			continue
		}
		var deliverErr error
		switch row.Channel {
		case "email":
			deliverErr = d.deliverEmail(ctx, row.URL, typ, payload, at)
		default:
			deliverErr = d.deliverWebhook(ctx, row.URL, row.SessionID, typ, payload, at)
		}
		if deliverErr != nil {
			_ = d.Store.OutboxRetry(ctx, row.ID, int64(row.Attempts))
			continue
		}
		_ = d.Store.OutboxDelivered(ctx, row.ID)
	}
	return nil
}

// deliverWebhook POST 事件 JSON（Slack/Feishu incoming webhook 兼容形态）。
func (d *Deliverer) deliverWebhook(ctx context.Context, target, sessionID, typ string, payload []byte, at time.Time) error {
	body, _ := json.Marshal(map[string]any{
		"session_id": sessionID, "type": typ, "payload": json.RawMessage(payload),
		"at": at.UTC().Format(time.RFC3339),
	})
	resp, err := d.HTTP.Post(target, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook %s: status %d", target, resp.StatusCode)
	}
	return nil
}

// deliverEmail SMTP 投递（正文 = 事件 JSON）。
func (d *Deliverer) deliverEmail(ctx context.Context, to, typ string, payload []byte, at time.Time) error {
	if d.SMTP.Host == "" {
		return fmt.Errorf("SMTP 未配置（SMTP_HOST）")
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: [chronotope] %s\r\n\r\n%s",
		d.SMTP.From, to, typ, string(payload))
	addr := net.JoinHostPort(d.SMTP.Host, d.SMTP.Port)
	return smtp.SendMail(addr, nil, d.SMTP.From, []string{to}, []byte(msg))
}

// validateDeliveryTarget 订阅校验：webhook 必须 http(s) 且非内网（SSRF 防护；
// AllowPrivate 仅供本地/e2e）；email 为邮箱格式。
func validateDeliveryTarget(channel, target string, allowPrivate bool) error {
	switch channel {
	case "email":
		if !strings.Contains(target, "@") || strings.ContainsAny(target, " \r\n") {
			return fmt.Errorf("email 地址无效")
		}
		return nil
	case "webhook", "":
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("url 必须为 http(s)://")
		}
		if allowPrivate {
			return nil
		}
		host := u.Hostname()
		if host == "localhost" || host == "host.docker.internal" {
			return fmt.Errorf("内网地址被拒（SSRF 防护）")
		}
		ips, err := net.LookupIP(host)
		if err != nil {
			return fmt.Errorf("域名解析失败: %w", err)
		}
		for _, ip := range ips {
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
				return fmt.Errorf("内网地址被拒（SSRF 防护）")
			}
		}
		return nil
	default:
		return fmt.Errorf("channel 仅支持 webhook|email")
	}
}
