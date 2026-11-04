package whatsappbridge

import (
	"errors"
	"os"
	"strconv"
	"time"
)

var errPlatform = errors.New("Host the WhatsApp Business webhook bridge on Linux; Windows desktops connect to it over HTTPS")

func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
func parseTimestamp(value string) (time.Time, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	return time.Unix(n, 0).UTC(), err
}
func privateRead(string, int64) ([]byte, error) { return nil, errPlatform }
func privateWrite(string, []byte) error         { return errPlatform }
func lockDirectory(string) (func(), error)      { return nil, errPlatform }
