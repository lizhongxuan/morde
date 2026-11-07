package app

import (
	"testing"

	"morde/server/internal/domain/task"
)

func TestRoutedFileDeliveryDirective(t *testing.T) {
	for _, test := range []struct {
		text         string
		workspace    bool
		want         task.FileDeliveryIntent
		wantExplicit bool
	}{
		{"生成 PDF 报告文件并保存到工作区", false, task.FileDeliveryRequired, true},
		{"做个 Excel 表格", false, task.FileDeliveryRequired, true},
		{"Create and save a PDF report", false, task.FileDeliveryRequired, true},
		{"修复项目代码", true, task.FileDeliveryRequired, true},
		{"不要生成文件，只要文字", false, task.FileDeliveryNone, true},
		{"别生成文件", false, task.FileDeliveryNone, true},
		{"No file needed; answer in chat", false, task.FileDeliveryNone, true},
		{"不要生成文件，但请创建 PDF 文件", false, task.FileDeliveryUnclear, true},
		{"不要解释错误，生成 PDF 文件", false, task.FileDeliveryRequired, true},
		{"帮我分析文件并写一段总结", false, "", false},
		{"请区别文件格式", false, "", false},
		{"输出文件内容", false, "", false},
		{"请告诉我如何生成 PDF 文件", false, "", false},
		{"把标题改成中文", false, "", false},
		{"写一份项目报告", false, "", false},
	} {
		t.Run(test.text, func(t *testing.T) {
			got, explicit := RoutedFileDeliveryDirective(test.text, test.workspace)
			if got != test.want || explicit != test.wantExplicit {
				t.Fatalf("directive=(%q,%t), want (%q,%t)", got, explicit, test.want, test.wantExplicit)
			}
		})
	}
}

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
