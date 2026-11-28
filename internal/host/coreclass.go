package host

import (
	"fmt"
	"strings"
)

// CoreClassesText describes the host's cores by efficiency class for the boot log and the problem report (Chinese,
// as both of them): one class is a processor without hybrid cores; several are listed slowest first.
func CoreClassesText(classes []CoreClass) string {
	switch len(classes) {
	case 0:
		return "读不到"
	case 1:
		return fmt.Sprintf("没有大小核（%d 核 %d 个逻辑处理器，效率等级都是 %d）", classes[0].Cores, classes[0].Logical,
			classes[0].Class)
	}
	parts := make([]string, len(classes))
	for k, c := range classes {
		parts[k] = fmt.Sprintf("等级 %d：%d 核 %d 个逻辑处理器", c.Class, c.Cores, c.Logical)
	}
	return "有大小核（效率等级越高越快）：" + strings.Join(parts, "；")
}
