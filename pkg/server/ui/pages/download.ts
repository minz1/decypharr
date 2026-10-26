import { bootPage, byId } from "#ui/lib/common/dom";
import { bindDownloadPage } from "#ui/lib/download/page";

bootPage("download", () => {
  const form = byId("downloadForm", HTMLFormElement);
  const debrid = document.querySelector("#debrid");
  bindDownloadPage(
    {
      form,
      magnetURI: byId("magnetURI", HTMLTextAreaElement),
      torrentFiles: byId("torrentFiles", HTMLInputElement),
      nzbURL: byId("nzbURL", HTMLTextAreaElement),
      nzbFile: byId("nzbFile", HTMLInputElement),
      arr: byId("arr", HTMLInputElement),
      downloadAction: byId("downloadAction", HTMLSelectElement),
      downloadUncached: byId("downloadUncached", HTMLInputElement),
      rmTrackerUrls: byId("rmTrackerUrls", HTMLInputElement),
      downloadFolder: byId("downloadFolder", HTMLInputElement),
      debrid: debrid instanceof HTMLSelectElement ? debrid : undefined,
      submit: byId("submitDownload", HTMLButtonElement),
    },
    form.dataset["downloadFolder"] ?? "",
  );
});
