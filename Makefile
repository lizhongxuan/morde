.PHONY: pc
pc:
	npm --prefix clients/desktop/harness/desktop run start -- --force-renderer-accessibility
