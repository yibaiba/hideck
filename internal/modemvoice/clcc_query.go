package modemvoice

import (
	"context"
	"errors"
	"fmt"
)

func (c *Client) queryCalls(ctx context.Context) ([]Call, error) {
	response, err := c.executor.ExecuteATContext(ctx, "AT+CLCC", queryTimeout)
	if err != nil {
		var partial interface{ PartialATResponse() string }
		if !errors.As(err, &partial) {
			return nil, fmt.Errorf("modem voice: CLCC: %w", err)
		}
		_, parseErr := ParseCLCC(partial.PartialATResponse())
		return nil, errors.Join(err, parseErr)
	}
	return ParseCLCC(response)
}
