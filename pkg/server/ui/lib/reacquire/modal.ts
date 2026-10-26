import { fetcher } from "#ui/lib/common/http";
import { createToast } from "#ui/lib/common/toast";
import { errorDetail, reacquireRequest } from "#ui/lib/reacquire/api";
import { loadJobs, searchBindings } from "#ui/lib/reacquire/loads";
import { markBinding } from "#ui/lib/reacquire/search";
import type { ReacquirePage } from "#ui/lib/reacquire/types";
import { describeError } from "#ui/lib/repair/dom";

const openReacquireModal = (page: ReacquirePage): void => {
  const { modal } = page;
  page.state.chosen = undefined;
  modal.query.value = "";
  modal.error.classList.add("hidden");
  modal.submit.disabled = true;
  modal.modal.showModal();
  void searchBindings(page);
};

const selectBinding = (page: ReacquirePage, index: number): void => {
  const binding = page.state.results[index];
  if (binding === undefined) {
    return;
  }
  page.state.chosen = binding;
  markBinding(page.modal.results, index);
  page.modal.submit.disabled = false;
};

const postReacquire = async (page: ReacquirePage, request: RequestInit): Promise<void> => {
  const { modal } = page;
  try {
    const response = await fetcher("/api/arr/reacquire", request);
    if (!response.ok) {
      throw new Error(errorDetail(await response.text(), response.status));
    }
    modal.modal.close();
    createToast("Reacquisition queued", "success");
    void loadJobs(page);
  } catch (error) {
    modal.error.textContent = describeError(error);
    modal.error.classList.remove("hidden");
  }
};

const submitReacquire = async (page: ReacquirePage): Promise<void> => {
  const { chosen } = page.state;
  if (chosen === undefined) {
    return;
  }
  const { modal } = page;
  modal.error.classList.add("hidden");
  modal.submit.disabled = true;
  await postReacquire(page, reacquireRequest(chosen, modal.strategy.value));
  modal.submit.disabled = false;
};

export { openReacquireModal, selectBinding, submitReacquire };
