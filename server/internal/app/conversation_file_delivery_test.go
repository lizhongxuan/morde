package app

import (
	"testing"
)

func TestUnverifiedFileDeliveryClaim(t *testing.T) {
	for _, test := range []struct {
		text string
		want bool
	}{
		{"我已经将文件保存到工作区。", true},
		{"PDF 已经成功生成，可以下载。", true},
		{"I have saved the file to the workspace.", true},
		{"The PDF was generated and saved.", true},
		{"我没有保存文件，当前无法使用工具。", false},
		{"文件尚未成功保存。", false},
		{"文件已无法保存。", false},
		{"我已经取消保存文件。", false},
		{"I have not saved the file.", false},
		{"你可以将文件保存到工作区。", false},
		{"请解释“文件已保存”是什么意思。", false},
		{"如何判断文件已经保存？", false},
		{"我已经生成了文字回答。", false},
	} {
		t.Run(test.text, func(t *testing.T) {
			if got := HasUnverifiedFileDeliveryClaim(test.text); got != test.want {
				t.Fatalf("claim=%t, want %t", got, test.want)
			}
		})
	}
}
