# The warm-pool launcher's make targets, included by the Makefile.
#
# ci-pr-checks.yaml runs these in warmpool-launcher-tests and, on the same
# paths filter, builds the launcher image (about 10 GB of vLLM base per
# platform). That filter lists this file, not the Makefile, so an edit to an
# unrelated target does not pull the image. Keep in here only what the launcher
# jobs run.
#
# The help line for test-warmpool-launcher stays in the Makefile, where
# `make help` lists it under Build.

# The launcher's own unit tests. They stub vLLM, so they need only the packages
# in warmpool/supervisor/tests/requirements.txt (pip install -r it first).
.PHONY: test-warmpool-launcher
test-warmpool-launcher:
	cd warmpool/supervisor && python3 -m pytest -v -p no:cacheprovider tests
