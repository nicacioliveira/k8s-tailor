import * as React from "react";

const ModalLayerContext = React.createContext(false);

function useInsideModal() {
  return React.useContext(ModalLayerContext);
}

function ModalLayerProvider({ children }: { children: React.ReactNode }) {
  return <ModalLayerContext.Provider value={true}>{children}</ModalLayerContext.Provider>;
}

const OPEN_MODAL_SELECTOR = [
  '[data-slot="dialog-content"][data-state="open"]',
  '[data-slot="sheet-content"][data-state="open"]',
  '[role="alertdialog"][data-state="open"]',
].join(",");

const modalListeners = new Set<() => void>();
let modalObserver: MutationObserver | null = null;

function subscribeModalOpen(listener: () => void) {
  modalListeners.add(listener);
  if (!modalObserver && typeof MutationObserver !== "undefined") {
    modalObserver = new MutationObserver(() => modalListeners.forEach((l) => l()));
    modalObserver.observe(document.body, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ["data-state"],
    });
  }
  return () => {
    modalListeners.delete(listener);
    if (modalListeners.size === 0) {
      modalObserver?.disconnect();
      modalObserver = null;
    }
  };
}

const getModalOpen = () => document.querySelector(OPEN_MODAL_SELECTOR) !== null;
const getModalOpenServer = () => false;

function useAnyModalOpen() {
  return React.useSyncExternalStore(subscribeModalOpen, getModalOpen, getModalOpenServer);
}

export { ModalLayerProvider, useAnyModalOpen, useInsideModal };
