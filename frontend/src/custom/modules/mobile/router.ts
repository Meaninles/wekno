import { createRouter, createWebHistory } from "vue-router";
import MobileChat from "./views/MobileChat.vue";
import MobileSettings from "./views/MobileSettings.vue";
import MobileKnowledgeManager from "./views/MobileKnowledgeManager.vue";

const routes = [
  {
    path: "/",
    redirect: "/chat",
  },
  {
    path: "/chat/:sessionId?",
    name: "mobile-chat",
    component: MobileChat,
  },
  {
    path: "/settings",
    name: "mobile-settings",
    component: MobileSettings,
  },
  {
    path: "/settings/knowledge",
    name: "mobile-knowledge",
    component: MobileKnowledgeManager,
  },
  {
    path: "/:pathMatch(.*)*",
    redirect: "/chat",
  },
];

export default createRouter({
  history: createWebHistory(),
  routes,
});
