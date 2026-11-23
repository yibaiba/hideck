package modem

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/yibaiba/hideck/pkg/logger"
)

const (
	ml307cSMSPromptTimeout = 5 * time.Second
	ml307cSMSResultTimeout = 60 * time.Second
)

func (m *Manager) sendML307CSMSPart(req commandRequest) error {
	select {
	case <-m.stop:
		return errors.New("AT 管理器停止")
	case m.cmdChanHigh <- req:
	case <-time.After(5 * time.Second):
		return errors.New("command queue full")
	}
	select {
	case err := <-req.errChan:
		return err
	case <-req.respChan:
		return nil
	case <-m.stopped:
		select {
		case err := <-req.errChan:
			return err
		case <-req.respChan:
			return nil
		default:
			return errors.New("AT 管理器停止")
		}
	}
}

// handleML307CSMS owns the AT queue from the command through the final
// response. No caller timeout can release the SMS lock mid-submission.
func (m *Manager) handleML307CSMS(req commandRequest) {
	start := time.Now()
	stage := "command"
	promptSeen, submitted := false, false
	written, mr, cms := 0, -1, -1
	report := func(err error) {
		fields := []any{
			"device", m.cfg.ID, "stage", stage, "prompt_seen", promptSeen,
			"planned_bytes", len(req.followUp), "written_bytes", written,
			"elapsed", time.Since(start).Truncate(time.Millisecond),
		}
		if mr >= 0 {
			fields = append(fields, "mr", mr)
		}
		if cms >= 0 {
			fields = append(fields, "cms_code", cms)
		}
		if errors.Is(err, ErrSMSSubmitUnknown) {
			logger.Warn("ML307C 短信提交结果未知", append(fields, "err", err)...)
		} else if err != nil {
			logger.Warn("ML307C 短信提交失败", append(fields, "err", err)...)
		} else {
			logger.Info("ML307C 短信提交已确认", fields...)
		}
	}
	fail := func(err error, recoverPort, cancelInput bool) {
		report(err)
		if recoverPort {
			if cancelInput {
				// Cancel is best effort. Retire this owner even if ESC writes.
				_, _ = m.port.Write([]byte{0x1B})
			}
			m.markUnhealthy()
			m.Stop()
			m.notifyDisconnect("ml307c_sms_unsynchronized")
		}
		req.errChan <- err
	}

	command := req.cmd + "\r"
	n, err := m.port.Write([]byte(command))
	if n != len(command) {
		err = errors.Join(err, io.ErrShortWrite)
	}
	if err != nil {
		fail(fmt.Errorf("写入 CMGS 命令失败 (%d/%d): %w", n, len(command), err), true, false)
		return
	}
	stage = "prompt"
	timer := time.NewTimer(req.timeout)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			if submitted {
				fail(ErrSMSSubmitUnknown, true, false)
			} else {
				fail(errors.New("等待 CMGS 短信输入提示超时"), true, true)
			}
			return
		case <-m.stop:
			err := errors.New("AT 管理器停止")
			if submitted {
				err = ErrSMSSubmitUnknown
			}
			fail(err, false, false)
			return
		case message := <-m.rxChan:
			if message.Err != nil {
				err := message.Err
				if submitted {
					err = ErrSMSSubmitUnknown
				}
				fail(err, true, false)
				return
			}
			line := strings.TrimSpace(message.Data)
			switch {
			case line == ">":
				if promptSeen {
					continue
				}
				stage = "submit"
				promptSeen = true
				written, err = m.port.Write([]byte(req.followUp))
				if written != len(req.followUp) {
					fail(errors.Join(errors.New("短信 PDU 写入未完成"), err, io.ErrShortWrite), true, true)
					return
				}
				if err != nil {
					fail(ErrSMSSubmitUnknown, true, false)
					return
				}
				submitted = true
				stage = "final"
				timer.Reset(req.smsFinalTimeout)
			case submitted && strings.HasPrefix(line, "+CMGS:"):
				if value, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "+CMGS:"))); err == nil {
					mr = value
				}
			case line == "OK":
				m.resetATTimeoutWatchdog()
				if !submitted {
					fail(errors.New("未收到 CMGS 短信输入提示"), false, false)
					return
				}
				if mr < 0 {
					fail(ErrSMSSubmitUnknown, false, false)
					return
				}
				report(nil)
				req.respChan <- fmt.Sprintf("+CMGS: %d", mr)
				return
			case line == "ERROR" || strings.HasPrefix(line, "+CMS ERROR:") || strings.HasPrefix(line, "+CME ERROR:"):
				m.resetATTimeoutWatchdog()
				if value, ok := strings.CutPrefix(line, "+CMS ERROR:"); ok {
					if code, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
						cms = code
					}
				}
				err := errors.New("模组拒绝 CMGS 短信提交")
				if cms >= 0 {
					err = fmt.Errorf("模组拒绝 CMGS 短信提交 (+CMS ERROR: %d)", cms)
				}
				fail(err, false, false)
				return
			default:
				if m.isURC(line) {
					m.handleURC(line)
				}
			}
		}
	}
}
