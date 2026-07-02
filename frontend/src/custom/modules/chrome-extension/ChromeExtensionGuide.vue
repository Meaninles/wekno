<template>
  <main class="chrome-extension-guide">
    <header class="guide-header">
      <div>
        <button type="button" class="guide-back" @click="goBack">
          <t-icon name="chevron-left" />
          <span>返回</span>
        </button>
        <h1>Chrome 插件离线安装指南</h1>
        <p>按图中红色编号操作：下载、解压、打开开发者模式、加载文件夹、回到 WeKnora 配置。</p>
      </div>
      <div class="guide-actions">
        <t-button theme="primary" @click="downloadChromeExtensionPackage">
          <template #icon><t-icon name="download" /></template>
          下载插件包
        </t-button>
        <t-button variant="outline" @click="configureNow">
          <template #icon><t-icon name="link" /></template>
          一键配置
        </t-button>
      </div>
    </header>

    <section class="guide-prerequisites" aria-label="准备事项">
      <div class="guide-prerequisites__item">
        <t-icon name="check-circle" />
        <span>使用 Chrome 浏览器完成安装。</span>
      </div>
      <div class="guide-prerequisites__item">
        <t-icon name="folder-open" />
        <span>插件包必须先解压，不能直接导入 zip。</span>
      </div>
      <div class="guide-prerequisites__item">
        <t-icon name="setting" />
        <span>Chrome 需要打开“开发者模式”后才能加载离线插件。</span>
      </div>
    </section>

    <section class="guide-checklist" aria-label="安装步骤">
      <article v-for="step in steps" :key="step.no" class="guide-step">
        <div class="guide-step__meta">
          <span class="guide-step__no">{{ step.no }}</span>
          <div>
            <h2>{{ step.title }}</h2>
            <p>{{ step.desc }}</p>
          </div>
        </div>
        <div class="guide-step__details">
          <div v-if="step.markers.length" class="guide-step__markers">
            <div v-for="marker in step.markers" :key="marker" class="guide-marker">
              {{ marker }}
            </div>
          </div>
          <p class="guide-step__result">{{ step.result }}</p>
        </div>
        <figure class="guide-step__figure">
          <img :src="step.img" :alt="step.title" />
          <figcaption>{{ step.caption }}</figcaption>
        </figure>
      </article>
    </section>

    <section class="guide-notes" aria-label="注意事项">
      <h2>安装限制</h2>
      <p>Chrome 离线安装不能直接选择 zip，也不能在未开启开发者模式时加载插件；如果 Chrome 提示插件来自外部来源，请确认文件夹来自本页下载的插件包。</p>
      <p>配置会写入当前 Chrome 插件本地配置；如果插件之前配置过其他空间，继续“一键配置”会覆盖为当前 WeKnora 空间。</p>
    </section>
  </main>
</template>

<script setup lang="ts">
import { useRouter } from 'vue-router'
import {
  downloadChromeExtensionPackage,
  oneClickConfigureChromeExtension,
} from './actions'

const router = useRouter()

const steps = [
  {
    no: 1,
    title: '下载插件包',
    desc: '在左下角账号菜单展开“Chrome 插件”，点击“插件下载”。',
    markers: ['红色 1：点击“插件下载”，浏览器会下载 weknora-chrome-extension.zip。'],
    result: '完成后：下载目录中出现 weknora-chrome-extension.zip。',
    caption: '第 1 步：从 WeKnora 直接下载离线插件包。',
    img: '/chrome-extension-guide/step-1-download.png',
  },
  {
    no: 2,
    title: '解压 zip',
    desc: '在下载目录找到 zip，右键选择“全部解压”。',
    markers: [
      '红色 1：选中下载到本地的 zip 文件。',
      '红色 2：右键菜单选择“全部解压”。',
    ],
    result: '完成后：得到一个解压后的插件文件夹，后续不要删除它。',
    caption: '第 2 步：Chrome 只能加载解压后的文件夹，不能直接加载 zip。',
    img: '/chrome-extension-guide/step-2-unzip.png',
  },
  {
    no: 3,
    title: '打开扩展管理页',
    desc: '在 Chrome 地址栏输入 chrome://extensions 并按 Enter。',
    markers: [
      '红色 1：地址栏输入 chrome://extensions。',
      '红色 2：确认输入无误后按 Enter。',
    ],
    result: '完成后：Chrome 打开“扩展程序”管理页。',
    caption: '第 3 步：进入 Chrome 扩展程序管理页。',
    img: '/chrome-extension-guide/step-3-open-extensions.png',
  },
  {
    no: 4,
    title: '打开开发者模式',
    desc: '在扩展管理页右上角打开“开发者模式”开关。',
    markers: ['红色箭头：打开右上角“开发者模式”。'],
    result: '完成后：页面顶部会出现“加载已解压的扩展程序”按钮。',
    caption: '第 4 步：必须开启开发者模式，Chrome 才允许加载离线插件。',
    img: '/chrome-extension-guide/step-4-developer-mode.png',
  },
  {
    no: 5,
    title: '加载已解压的插件',
    desc: '点击“加载已解压的扩展程序”，选择第 2 步解压出的插件文件夹。',
    markers: [
      '红色 1：点击“加载已解压的扩展程序”。',
      '红色 2：选择解压后的插件文件夹。',
      '红色 3：点击“选择文件夹”。',
    ],
    result: '完成后：扩展列表中出现“知识管理助手-你的第二大脑，从浏览器开始”。',
    caption: '第 5 步：选择的是解压后的插件文件夹，不是 zip 文件。',
    img: '/chrome-extension-guide/step-5-load-unpacked.png',
  },
  {
    no: 6,
    title: '回到 WeKnora 一键配置',
    desc: '回到 WeKnora，展开“Chrome 插件”，点击“一键配置”。',
    markers: ['红色 6：点击“一键配置”，WeKnora 会检测插件并写入当前空间配置。'],
    result: '完成后：看到“Chrome 插件已配置到当前空间”的成功提示；如果已有配置，会先提示是否覆盖。',
    caption: '第 6 步：安装完成后回到 WeKnora 进行当前空间绑定。',
    img: '/chrome-extension-guide/step-6-configure.png',
  },
]

function goBack() {
  if (window.history.length > 1) {
    router.back()
    return
  }
  router.push('/platform/knowledge-bases')
}

function configureNow() {
  oneClickConfigureChromeExtension(router)
}
</script>

<style scoped lang="less">
.chrome-extension-guide {
  flex: 1 1 auto;
  height: 100%;
  min-height: 0;
  box-sizing: border-box;
  overflow-x: hidden;
  overflow-y: auto;
  overscroll-behavior: contain;
  padding: 24px 32px 40px;
  background: var(--td-bg-color-page);
  color: var(--td-text-color-primary);
}

.guide-header {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 24px;
  max-width: 1120px;
  margin: 0 auto 20px;

  h1 {
    margin: 10px 0 6px;
    font-size: 26px;
    line-height: 1.25;
    font-weight: 650;
    letter-spacing: 0;
  }

  p {
    margin: 0;
    font-size: 14px;
    color: var(--td-text-color-secondary);
  }
}

.guide-back {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 0;
  border: 0;
  background: transparent;
  color: var(--td-text-color-secondary);
  cursor: pointer;
  font-size: 14px;

  &:hover {
    color: var(--td-brand-color);
  }
}

.guide-actions {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-shrink: 0;
}

.guide-prerequisites {
  max-width: 1120px;
  margin: 0 auto 16px;
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 10px;
}

.guide-prerequisites__item {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
  padding: 10px 12px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  background: var(--td-bg-color-container);
  color: var(--td-text-color-secondary);
  font-size: 13px;
  line-height: 1.5;

  .t-icon {
    flex-shrink: 0;
    color: var(--td-brand-color);
  }
}

.guide-checklist {
  max-width: 1120px;
  margin: 0 auto;
  display: grid;
  gap: 16px;
}

.guide-step {
  display: grid;
  grid-template-columns: 280px minmax(0, 1fr);
  gap: 18px;
  align-items: start;
  padding: 16px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  background: var(--td-bg-color-container);
}

.guide-step__meta {
  display: flex;
  gap: 12px;
  min-width: 0;

  h2 {
    margin: 1px 0 6px;
    font-size: 16px;
    line-height: 1.35;
    font-weight: 650;
    letter-spacing: 0;
  }

  p {
    margin: 0;
    font-size: 13px;
    line-height: 1.7;
    color: var(--td-text-color-secondary);
  }
}

.guide-step__details {
  grid-column: 1;
  display: grid;
  gap: 10px;
  min-width: 0;
  padding-left: 40px;
}

.guide-step__markers {
  display: grid;
  gap: 6px;
}

.guide-marker {
  padding: 7px 9px;
  border-radius: 6px;
  background: var(--td-error-color-light);
  color: var(--td-text-color-primary);
  font-size: 12px;
  line-height: 1.55;
}

.guide-step__result {
  margin: 0;
  padding: 7px 9px;
  border-radius: 6px;
  background: var(--td-success-color-light);
  color: var(--td-text-color-primary);
  font-size: 12px;
  line-height: 1.55;
}

.guide-step__no {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  width: 28px;
  height: 28px;
  border-radius: 50%;
  background: var(--td-brand-color);
  color: var(--td-text-color-anti);
  font-size: 14px;
  font-weight: 700;
}

.guide-step__figure {
  grid-column: 2;
  grid-row: 1 / span 2;
  margin: 0;
  min-width: 0;
  overflow: hidden;
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
  background: #f3f4f6;

  img {
    display: block;
    width: 100%;
    height: auto;
  }

  figcaption {
    padding: 9px 11px;
    border-top: 1px solid var(--td-component-stroke);
    background: var(--td-bg-color-container);
    color: var(--td-text-color-secondary);
    font-size: 12px;
    line-height: 1.5;
  }
}

.guide-notes {
  max-width: 1120px;
  margin: 16px auto 0;
  padding: 14px 16px;
  border: 1px solid var(--td-warning-color-3);
  border-radius: 8px;
  background: var(--td-warning-color-1);

  h2 {
    margin: 0 0 8px;
    font-size: 15px;
    line-height: 1.4;
    font-weight: 650;
    letter-spacing: 0;
  }

  p {
    margin: 0;
    color: var(--td-text-color-secondary);
    font-size: 13px;
    line-height: 1.7;
  }

  p + p {
    margin-top: 4px;
  }
}

@media (max-width: 900px) {
  .chrome-extension-guide {
    padding: 18px 14px 28px;
  }

  .guide-header {
    align-items: stretch;
    flex-direction: column;
    gap: 14px;

    h1 {
      font-size: 22px;
    }
  }

  .guide-actions {
    flex-wrap: wrap;
  }

  .guide-prerequisites {
    grid-template-columns: 1fr;
  }

  .guide-step {
    grid-template-columns: 1fr;
  }

  .guide-step__details {
    grid-column: 1;
    padding-left: 0;
  }

  .guide-step__figure {
    grid-column: 1;
    grid-row: auto;
  }
}
</style>
