import { createRouter, createWebHistory } from "vue-router";
import MobileChat from "./views/MobileChat.vue";
import MobileSettings from "./views/MobileSettings.vue";
import MobileKnowledgeManager from "./views/MobileKnowledgeManager.vue";
import ChatShareSelectView from "../chatshare/views/ChatShareSelectView.vue";

const routes = [
  {
    path: "/",
    redirect: "/chat",
  },
  {
    path: "/chat/:sessionId/share",
    name: "mobile-chat-share",
    component: ChatShareSelectView,
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
  history: createWebHistory(import.meta.env.BASE_URL),
  routes,
});
