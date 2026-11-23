package device

import (
	"fmt"
	"time"
)

func configureWorkerATSMS(w *Worker) {
	if w == nil {
		return
	}
	w.smsMode = smsModeAT
	if w.Modem == nil {
		return
	}
	w.Modem.SetNewSMSHandler(nil)
	w.Modem.SetDisableURCRead(false)
	if w.atDeviceRules().storedSMSInbox {
		w.Modem.SetStoredSMSHandler(w.handleStoredSMS)
		return
	}
	w.Modem.SetSMSReadinessCheck(func() error {
		_, err := w.resolveSMSIdentity()
		return err
	})
	w.Modem.SetSMSProcessor(w.processSMS)
}

func resumeWorkerATSMS(w *Worker) error {
	configureWorkerATSMS(w)
	if w == nil {
		return fmt.Errorf("worker 为空")
	}
	if w.Modem == nil {
		return fmt.Errorf("设备 %s 没有 AT 短信调度器", w.ID)
	}
	if !w.Modem.CanExecuteAT() {
		return fmt.Errorf("设备 %s 的 AT 短信调度器未运行", w.ID)
	}
	_, err := w.Modem.ExecuteATSilent("AT+CNMI=2,1,0,0,0", 2*time.Second)
	return err
}
