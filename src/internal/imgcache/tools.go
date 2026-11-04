package imgcache

import (
  "crypto/md5"
  "encoding/hex"
)

func strToMD5(str string) string {
  md5Hasher := md5.New()
  md5Hasher.Write([]byte(str))
  return hex.EncodeToString(md5Hasher.Sum(nil))
}
