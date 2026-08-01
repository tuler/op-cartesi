# Cartesi Accounts Drive Format v1 — C99 library.
#
# `make test` regenerates the golden-vector header from
# ../testdata/golden.json (via test/gen_golden.py, python3 stdlib only),
# builds the test binary, and runs it.
#
# Paths are relative to this directory; run as `make -C accounts-drive/c test`
# or from within it.

CC ?= cc
CFLAGS ?= -O2 -Wall -Wextra -Werror
CFLAGS += -std=c99
PYTHON ?= python3

GOLDEN = ../testdata/golden.json
BUILD = build

all: $(BUILD)/test_accounts_drive

$(BUILD):
	mkdir -p $(BUILD)

$(BUILD)/golden_vectors.h: test/gen_golden.py $(GOLDEN) | $(BUILD)
	$(PYTHON) test/gen_golden.py $(GOLDEN) $@

$(BUILD)/test_accounts_drive: test/test_main.c accounts_drive.c \
		accounts_drive.h $(BUILD)/golden_vectors.h | $(BUILD)
	$(CC) $(CFLAGS) -I. -I$(BUILD) -o $@ test/test_main.c accounts_drive.c

test: $(BUILD)/test_accounts_drive
	$(BUILD)/test_accounts_drive

clean:
	rm -rf $(BUILD)

.PHONY: all test clean
.DELETE_ON_ERROR:
