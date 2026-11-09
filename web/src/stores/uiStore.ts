import { create } from 'zustand';

interface UIState {
  sidebarCollapsed: boolean;
  mobileNavOpen: boolean;
  lastNonChatPath: string;
  setSidebarCollapsed: (collapsed: boolean) => void;
  setMobileNavOpen: (open: boolean) => void;
  setLastNonChatPath: (path: string) => void;
}

// CONTRACT.md 8.5：只存纯 UI 状态，不存 API 数据。
export const useUIStore = create<UIState>((set) => ({
  sidebarCollapsed: false,
  mobileNavOpen: false,
  lastNonChatPath: '/chat',
  setSidebarCollapsed: (collapsed) => set({ sidebarCollapsed: collapsed }),
  setMobileNavOpen: (open) => set({ mobileNavOpen: open }),
  setLastNonChatPath: (path) => set({ lastNonChatPath: path }),
}));
