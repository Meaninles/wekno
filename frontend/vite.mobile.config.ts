import { fileURLToPath, URL } from "node:url";
import { resolve, dirname } from "node:path";
import { execSync } from "node:child_process";
import { createRequire } from "node:module";
import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import vueJsx from "@vitejs/plugin-vue-jsx";

const __dirname = dirname(fileURLToPath(import.meta.url));
const require = createRequire(import.meta.url);
const pkg = require("./package.json") as { version?: string };

function resolveFrontendCommit(): string {
  const fromEnv = process.env.VITE_FRONTEND_COMMIT || process.env.GITHUB_SHA;
  if (fromEnv) return fromEnv.slice(0, 7);
  try {
    return execSync("git rev-parse --short HEAD", { stdio: ["ignore", "pipe", "ignore"] })
      .toString()
      .trim();
  } catch {
    return "unknown";
  }
}

const DEV_PROXY_TARGET =
  process.env.VITE_DEV_PROXY_TARGET ||
  process.env.FRONTEND_BACKEND_URL ||
  "http://localhost:8080";

function configureForwardedHeaders(proxy: any) {
  proxy.on("proxyReq", (proxyReq: any, req: any) => {
    const host = req.headers?.host;
    if (host) proxyReq.setHeader("X-Forwarded-Host", host);
    const encrypted = !!req.socket?.encrypted;
    proxyReq.setHeader("X-Forwarded-Proto", encrypted ? "https" : "http");
  });
}

export default defineConfig({
  define: {
    __FRONTEND_VERSION__: JSON.stringify(pkg.version ?? "unknown"),
    __FRONTEND_COMMIT__: JSON.stringify(resolveFrontendCommit()),
  },
  build: {
    outDir: "dist-mobile",
    emptyOutDir: true,
    rollupOptions: {
      input: {
        mobile: resolve(__dirname, "mobile.html"),
      },
      output: {
        manualChunks(id) {
          if (!id.includes("node_modules")) return;
          if (id.includes("marked") || id.includes("katex")) return "vendor-markdown";
          if (id.includes("tdesign")) return "vendor-tdesign";
        },
      },
    },
  },
  plugins: [vue(), vueJsx()],
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  optimizeDeps: {
    exclude: ["tdesign-icons-vue-next"],
  },
  server: {
    port: 5178,
    host: true,
    proxy: {
      "/api": {
        target: DEV_PROXY_TARGET,
        changeOrigin: true,
        secure: false,
        configure: configureForwardedHeaders,
      },
      "/files": {
        target: DEV_PROXY_TARGET,
        changeOrigin: true,
        secure: false,
        configure: configureForwardedHeaders,
      },
    },
  },
});
