// 生成 Tauri updater 所需的 latest.json。
// 用法: node .github/scripts/gen-latest-json.mjs <tag>
// 扫描 NSIS 产物目录中的安装包与其 .sig 签名，输出 latest.json 到同一目录。
import fs from "node:fs";
import path from "node:path";

const tag = process.argv[2];
if (!tag) {
  console.error("用法: node gen-latest-json.mjs <tag>");
  process.exit(1);
}

const repo = process.env.GITHUB_REPOSITORY ?? "coconilu/localmap";
const nsisDir = "ui-tauri/src-tauri/target/release/bundle/nsis";
const version = JSON.parse(fs.readFileSync("ui-tauri/package.json", "utf8")).version;

const files = fs.readdirSync(nsisDir);
const exe = files.find((f) => f.endsWith(".exe") && !f.endsWith(".sig"));
if (!exe) {
  console.error(`未在 ${nsisDir} 找到安装包`);
  process.exit(1);
}
const sigFile = `${exe}.sig`;
if (!files.includes(sigFile)) {
  console.error(`未找到签名文件 ${sigFile}（TAURI_SIGNING_PRIVATE_KEY 未生效？）`);
  process.exit(1);
}

const latest = {
  version,
  notes: `LocalMap ${tag}`,
  pub_date: new Date().toISOString(),
  platforms: {
    "windows-x86_64": {
      signature: fs.readFileSync(path.join(nsisDir, sigFile), "utf8").trim(),
      url: `https://github.com/${repo}/releases/download/${tag}/${encodeURIComponent(exe)}`,
    },
  },
};

const out = path.join(nsisDir, "latest.json");
fs.writeFileSync(out, JSON.stringify(latest, null, 2));
console.log(`已生成 ${out}`);
