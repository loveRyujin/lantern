#!/bin/sh
# 假的 python3。测试通过 PYRT_FAKE_* 和旁边的 .ver 文件控制版本与失败。
PATH="/usr/bin:/bin${PATH:+:$PATH}"
export PATH

if [ -n "${PYRT_FAKE_LOG:-}" ]; then
	printf '%s\n' "$0 $*" >> "${PYRT_FAKE_LOG}"
fi

if [ "${PYRT_FAKE_SLEEP:-}" = "1" ]; then
	sleep 30
fi

mode=""
req=""
prev=""
last=""
for arg in "$@"; do
	if [ "$prev" = "-m" ]; then
		mode=$arg
	fi
	if [ "$prev" = "-r" ]; then
		req=$arg
	fi
	prev=$arg
	last=$arg
done

if [ "$mode" = "venv" ]; then
	if [ -z "$last" ]; then
		echo "missing venv dest" >&2
		exit 2
	fi
	for arg in "$@"; do
		if [ "$arg" = "--clear" ]; then
			rm -rf "$last"
		fi
	done
	mkdir -p "$last/bin"
	cp "$0" "$last/bin/python"
	chmod 755 "$last/bin/python"
	if [ -f "$0.ver" ]; then
		cp "$0.ver" "$last/bin/python.ver"
	fi
	exit 0
fi

if [ "$mode" = "pip" ]; then
	if [ "${PYRT_FAKE_PIP_FAIL:-}" = "1" ]; then
		echo "ERROR: hash mismatch" >&2
		exit 1
	fi
	case " $* " in
	*" --require-hashes "*) ;;
	*)
		echo "missing --require-hashes" >&2
		exit 2
		;;
	esac
	case " $* " in
	*" --only-binary=:all: "*) ;;
	*)
		echo "missing --only-binary" >&2
		exit 2
		;;
	esac
	if [ -z "$req" ] || [ ! -f "$req" ]; then
		echo "missing requirements" >&2
		exit 2
	fi
	pandas=0
	mpl=0
	hashed=0
	while IFS= read -r line; do
		case "$line" in
		pandas==*) pandas=1 ;;
		matplotlib==*) mpl=1 ;;
		*"--hash=sha256:"*) hashed=1 ;;
		esac
	done < "$req"
	if [ "$pandas" != 1 ] || [ "$mpl" != 1 ] || [ "$hashed" != 1 ]; then
		echo "requirements not hash-locked" >&2
		exit 2
	fi
	exit 0
fi

if [ "${PYRT_FAKE_EXIT:-}" = "1" ]; then
	echo "boom" >&2
	exit 1
fi

if [ "${PYRT_FAKE_GARBAGE:-}" = "1" ]; then
	printf '%s\n' "not-json"
	exit 0
fi

ver="3.12.1"
if [ -f "$0.ver" ]; then
	ver=$(tr -d '[:space:]' < "$0.ver")
fi
oldifs=$IFS
IFS=.
# shellcheck disable=SC2086
set -- $ver
IFS=$oldifs
major=$1
minor=$2
micro=${3:-0}

exe=$0
case $exe in
/*) ;;
*)
	exe=$(CDPATH= cd "$(dirname "$0")" && pwd)/$(basename "$0")
	;;
esac
prefix=${PYRT_FAKE_PREFIX:-/opt/lantern-python}
printf '{"major":%s,"minor":%s,"micro":%s,"executable":"%s","base_prefix":"%s","stdlib":"%s"}\n' \
	"$major" "$minor" "$micro" "$exe" "$prefix" "$prefix/lib/python${major}.${minor}"
