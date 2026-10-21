package outsidecall

// RecordExportApproval 是同名的本地函数，不是种子里的那个。
func RecordExportApproval() {}

func callLocal() {
	RecordExportApproval()
}
