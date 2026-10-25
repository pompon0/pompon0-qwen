set -euo pipefail
[[ ! -e $1 ]] || { echo 'FAIL: host canary visible'; exit 1; }
opts=$(findmnt -n -o VFS-OPTIONS -T /usr)
[[ ,$opts, == *,ro,* ]] || { echo 'FAIL: /usr is not read-only'; exit 1; }
while read -r name value rest; do
  case $name in
    CapInh:|CapPrm:|CapEff:|CapBnd:|CapAmb:)
      [[ $value == 0000000000000000 ]] || { echo "FAIL: $name is nonzero"; exit 1; } ;;
  esac
done < /proc/self/status
if nft list ruleset >/dev/null 2>&1; then
  echo 'FAIL: workload has firewall authority'; exit 1
fi
if unshare --user --map-root-user /usr/bin/true >/dev/null 2>&1; then
  echo 'FAIL: workload can create further user namespaces'; exit 1
fi
p=$(mktemp /workspace/.qwen-isolation-check.XXXXXXXX); rm -- "$p"
p=$(mktemp "$HOME/.qwen-isolation-check.XXXXXXXX"); rm -- "$p"
/opt/node/bin/node --version
/opt/node/bin/qwen --version
/opt/node/bin/node --input-type=module -e '
import net from "node:net";
const connect = (host, port) => new Promise((resolve, reject) => {
  const s = net.createConnection({host, port});
  s.setTimeout(8000, () => s.destroy(new Error("connection timed out")));
  s.once("connect", () => {s.destroy(); resolve();});
  s.once("error", reject);
});
try {
  await connect("198.18.0.1", 9);
  throw new Error("blocked destination was reachable");
} catch (e) {
  if (!["EACCES", "EPERM", "EHOSTUNREACH", "ENETUNREACH"].includes(e.code)) throw e;
}
await connect("1.1.1.1", 443);
console.log("PASS: denied destination rejected; public IPv4 TCP works.");
'
echo 'PASS: host canary hidden; /usr read-only; writable project/home; no capabilities or firewall authority.'
