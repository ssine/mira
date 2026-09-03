import { execFileSync } from "node:child_process";
const binary = process.argv[2];
if (!binary) throw new Error("usage: node scripts/check-android-build.mjs <android-binary>");
const metadata = execFileSync("go", ["version", "-m", binary], { encoding: "utf8" });
for (const setting of ["CGO_ENABLED=1", "GOOS=android", "GOARCH=arm64", "-tags=netcgo"]) {
  if (!metadata.includes(setting)) throw new Error(`Android build is missing ${setting}; domain DNS requires the system resolver`);
}
console.log("Android arm64 build uses the native DNS resolver (NDK/cgo).");
