SURVIVAL AI  -  {{PRODUCT}}
===============================================================================

An offline library and a local AI assistant, on this drive.
No internet. No installation. No account. Nothing is sent anywhere.


WHAT IS ON THE DRIVE
--------------------------------------------------------------------------

  START_SURVIVAL_AI.exe   double-click this
  README.txt              this file
  archives\               {{BOOKS}} offline books ({{BOOKS_GIB}})
  models\                 {{MODELS}} local AI models ({{MODELS_GIB}})
  bin\                    the programs that serve them
  tools\                  the tool that built this drive, plus its source
  LICENSES\               what belongs to whom, and under what terms
  _MANIFEST.sha256        a checksum for every file, so you can prove the
                          drive is intact (see "Checking your drive")
  image_version.txt       what this particular build contains


GETTING STARTED
--------------------------------------------------------------------------

  1. Plug the drive into a Windows PC.
  2. Double-click START_SURVIVAL_AI.exe
  3. Your browser opens a dashboard. Click any section.

The library is ready in a few seconds. Sections open in their own browser
tab, so you can keep several open at once.

The assistant (Survival Advisor) may show "Start assistant" instead of
"Open". That is deliberate: loading a multi-gigabyte model is heavy, so it
only happens when you ask. Click it and wait - on a USB stick the first
load takes one to three minutes. Closing the app frees the memory again.

Keep the console window open while you use the drive. Closing it stops the
library and the assistant. Your browser tabs stay readable, but new pages
will not load.

To stop everything: close the console window, or press Ctrl-C in it.


MINIMUM REQUIREMENTS
--------------------------------------------------------------------------

  Operating system   Windows 10 (64-bit) or Windows 11
  Processor          Any 64-bit Intel or AMD processor.
                     AVX2 is strongly recommended for the assistant; without
                     it the assistant is very slow but the library is fine.
  Memory             2 GB free for the library.
                     Plus 7 GB free for the assistant with this model.
                     (The launcher refuses the assistant if there is not
                     enough free memory, and says so.)
  Disk               This drive. Nothing is installed on the PC.
  Graphics card      Optional. If Vulkan is available it is used; otherwise
                     the assistant runs on the CPU.
  Browser            Any current browser (Edge, Chrome, Firefox).
                     JavaScript must be allowed for 127.0.0.1.
  Administrator      Not required.
  Internet           Not required, not used.

Windows on ARM can run this under x64 emulation; it is slower and not
tested.


WHAT WORKS WHILE THE ASSISTANT IS LOADING
--------------------------------------------------------------------------

Everything in the library, always. The library never waits for the assistant
and does not depend on it.

When you start the assistant, its card changes from "Starting..." to "Open"
when it is ready, and you can keep browsing every section while it loads. If
the assistant cannot run on your machine, the library is unaffected and the
card simply says why.


CHECKING YOUR DRIVE
--------------------------------------------------------------------------

Every file on this drive is listed in _MANIFEST.sha256. To verify the whole
drive, open a Command Prompt in this folder and run:

    tools\survival-drive.exe verify -drive G:

(replace G: with the drive letter). A full check reads every byte and takes
about half an hour at USB speeds. A quick size-only check:

    tools\survival-drive.exe verify -drive G: -quick

The dashboard's "Check drive integrity" button does the quick check.

This matters: it is how you know the drive survived shipping, and how you
would prove a copy is genuine years from now. The check needs no internet,
no cache and no other files.


REBUILDING THIS DRIVE YOURSELF
--------------------------------------------------------------------------

tools\ contains the builder used to produce this drive, the manifest it
built from, and its complete source code. Anyone can reproduce or audit it.

  tools\survival-drive.exe    the builder (Windows x64, no dependencies)
  tools\source.zip            its full source (Go, standard library only)
  tools\{{LOCKFILE}}          the exact file list this drive was built from,
                              including a sha256 for every file

To re-download the same content and rebuild an identical drive:

    tools\survival-drive.exe fetch -lock tools\{{LOCKFILE}} -cache D:\cache
    tools\survival-drive.exe build -lock tools\{{LOCKFILE}} -out D:\image
    tools\survival-drive.exe burn  -image D:\image -drive E:

The lock file pins every file by size and sha256, so the rebuilt drive can be
compared with this one byte for byte. If a source ever changes upstream, the
tool refuses to download it and tells you to resolve again rather than
silently producing a different drive.

Other useful commands:

    tools\survival-drive.exe resolve -profile profiles\sku1.json -variant sell
    tools\survival-drive.exe verify -lock tools\{{LOCKFILE}}

Everything the builder downloads comes from public archives: Kiwix
(Wikipedia, Project Gutenberg, Stack Exchange, MedlinePlus, Wikibooks,
Wikivoyage, Appropedia, energypedia), Hugging Face (the AI models), and
GitHub (llama.cpp). Nothing proprietary is fetched from anywhere.

The builder is also published at {{REPO}}, which is where fixes and newer
releases appear.


IF SOMETHING DOES NOT WORK
--------------------------------------------------------------------------

  The dashboard does not open
      The port is chosen per launch. Look at the console window: it prints
      "opening http://127.0.0.1:NNNNN". Copy that address into the browser.

  Pages stop loading after a while
      The console window was probably closed; the library stops with it.
      Run START_SURVIVAL_AI.exe again.

  The assistant is slow or absent
      It needs memory and, ideally, AVX2 and a Vulkan-capable GPU. Start it
      from its card when you want it, and stop it when you do not. The
      library is unaffected either way.

  Something else
      logs\launcher.log records the decisions this launch made: memory,
      chosen model, ports, which service failed and why.


LICENSING
--------------------------------------------------------------------------

This drive contains public content and open-source software. Nothing here
grants you rights beyond what those licences allow.

  LICENSES\NOTICE.md   what is included, who made it, under which licence
  LICENSES\*.txt       the licence texts themselves

The encyclopedias and reference works are mostly CC BY-SA: you may copy and
share them, and if you adapt them, the result must carry the same licence.
Project Gutenberg texts are public domain in the United States. The AI
models remain under their own terms (Gemma Terms of Use, Apache 2.0).

This project's own code is GPL-3.0-or-later: its source is in
tools\source.zip and at {{REPO}}. The servers are separate programs:
llama.cpp (MIT) and Kiwix (GPLv3). LICENSES\NOTICE.md names the exact source
location for each GPL component and makes the written source offer.


BUILD
--------------------------------------------------------------------------

  version     {{VERSION}}
  profile     {{SKU}}
  built       {{BUILT}}
  content     {{GIB}}
  licences    {{LICENSES}}

===============================================================================
