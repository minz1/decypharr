const HALF = 2;

const scrollTarget = (tab: { left: number; width: number }, scrollerWidth: number): number =>
  Math.max(0, tab.left - (scrollerWidth - tab.width) / HALF);

const centre = (tab: HTMLElement): void => {
  const scroller = tab.closest('[role="tablist"]')?.parentElement;
  if (scroller && scroller.scrollWidth > scroller.clientWidth) {
    scroller.scrollTo({
      left: scrollTarget({ left: tab.offsetLeft, width: tab.offsetWidth }, scroller.clientWidth),
      behavior: "auto",
    });
  }
};

const bindTabs = (tablist: HTMLElement, panelsRoot: HTMLElement): void => {
  const tabs = [...tablist.querySelectorAll<HTMLElement>('[role="tab"]')];
  const panels = [...panelsRoot.querySelectorAll<HTMLElement>(".tab-content")];
  for (const tab of tabs) {
    tab.addEventListener("click", () => {
      for (const other of tabs) {
        other.classList.remove("tab-active");
      }
      tab.classList.add("tab-active");
      centre(tab);
      for (const panel of panels) {
        panel.classList.toggle("hidden", panel.id !== `tab-${tab.dataset["tab"] ?? ""}`);
      }
    });
  }
};

export { bindTabs, scrollTarget };
