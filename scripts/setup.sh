set -euo pipefail
nft=$1; policy=$2; ready_fd=$3; start_fd=$4
shift 4
"$nft" -f "$policy"
printf 1 >&"$ready_fd"
exec {ready_fd}>&-
IFS= read -r start <&"$start_fd"
exec {start_fd}<&-
[[ $start == go ]]
exec "$@"
