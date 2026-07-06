import "@/custom/modules/consoleFilter/install";
import { installSafeMessagePlugin } from "@/custom/modules/safeMessage/install";
import { createApp } from "vue";
import { createPinia } from "pinia";
import TDesign from "tdesign-vue-next";
import "tdesign-vue-next/es/style/index.css";
import "@/assets/theme/theme.css";
import "@/components/css/chat-hljs-dark.less";
import "vue-virtual-scroller/dist/vue-virtual-scroller.css";
import i18n from "./i18n";
import { initTheme } from "@/composables/useTheme";
import { installTDesignIconOfflineGuard } from "@/utils/tdesign-icon-offline";
import MobileApp from "@/custom/modules/mobile/MobileApp.vue";
import MobileIcon from "@/custom/modules/mobile/components/MobileIcon.vue";
import mobileRouter from "@/custom/modules/mobile/router";

installTDesignIconOfflineGuard();

initTheme();

const app = createApp(MobileApp);

app.use(TDesign);
app.component("MobileIcon", MobileIcon);
installSafeMessagePlugin(app);
app.use(createPinia());
app.use(mobileRouter);
app.use(i18n);

mobileRouter.isReady().finally(() => {
  app.mount("#mobile-app");
});
