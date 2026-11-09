import { create } from 'zustand';

interface ComposerState {
  drafts: Record<string, string>;
  setDraft: (conversationId: string, value: string) => void;
  clearDraft: (conversationId: string) => void;
}

// CONTRACT.md 8.5：只存未发送文本，不持久化。
export const useComposerStore = create<ComposerState>((set) => ({
  drafts: {},
  setDraft: (conversationId, value) =>
    set((state) => ({ drafts: { ...state.drafts, [conversationId]: value } })),
  clearDraft: (conversationId) =>
    set((state) => {
      const drafts = { ...state.drafts };
      delete drafts[conversationId];
      return { drafts };
    }),
}));
