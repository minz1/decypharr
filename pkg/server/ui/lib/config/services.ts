import { collectArrs, populateArrs } from "#ui/lib/config/arrs";
import { collectDebrids, populateDebrids } from "#ui/lib/config/debrids";
import { collectQueueCleanup, populateQueueCleanup } from "#ui/lib/config/queue-cleanup";
import type { Section } from "#ui/lib/config/section";
import { collectUsenet, populateUsenet } from "#ui/lib/config/usenet";
import { collectVirtualFolders, populateVirtualFolders } from "#ui/lib/config/virtual-folders";

const serviceSections: readonly Section[] = [
  { prefix: "debrids", populate: populateDebrids, collect: collectDebrids },
  { prefix: "usenet", populate: populateUsenet, collect: collectUsenet },
  { prefix: "virtual_folders", populate: populateVirtualFolders, collect: collectVirtualFolders },
  { prefix: "arrs", populate: populateArrs, collect: collectArrs },
  { prefix: "queue_cleanup", populate: populateQueueCleanup, collect: collectQueueCleanup },
];

export { serviceSections };
