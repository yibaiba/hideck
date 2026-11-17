package modem

// A failed CLCC is still a failure. Keep its partial response available to the
// caller for an explicit retry, without putting call numbers into error logs.
type clccResponseError struct{ response, terminal string }

func (e *clccResponseError) Error() string             { return "CLCC 设备返回错误: " + e.terminal }
func (e *clccResponseError) PartialATResponse() string { return e.response }
