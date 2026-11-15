# Code signing policy

Free code signing provided by [SignPath.io](https://about.signpath.io), certificate by [SignPath Foundation](https://signpath.org).

The Windows release files (`zoo_<version>_windows_amd64.zip` and `zoo_<version>_windows_arm64.zip`) contain a signed `zoo.exe`. The signature shows "SignPath Foundation" as the publisher. Windows features such as Smart App Control use the signature to decide if a program can run.

## What is signed

Only `zoo.exe` from the [releases page](https://github.com/dustywusty/zoo/releases) is signed. GitHub Actions builds it from a release tag in this repository, with the workflow in `.github/workflows/release.yml`. The workflow sends the exe to SignPath. SignPath signs a file only if these conditions are true:

- The file comes from this repository and from a workflow that ran on GitHub-hosted runners.
- The product name in the file is `zoo`, and the product version matches the release.
- An approver accepts the signing request for that release.

A `zoo.exe` that you build yourself is not signed.

## Team roles

- Committers and reviewers: [repository owner](https://github.com/dustywusty). Every change from a person outside the team gets a review before it is merged. This includes changes to the build scripts and the CI workflows.
- Approvers: [repository owner](https://github.com/dustywusty). An approver accepts each signing request by hand.

All team members use multi-factor authentication for GitHub and for SignPath.

## Privacy policy

This program will not transfer any information to other networked systems unless specifically requested by the user or the person installing or operating it.

zoo runs AI coding agents that you start. The agents connect to the services that you allow, for example a model API. zoo itself does not send data to the zoo project or to any other party.

## Report a problem

If you think that a signed file is not from this project, or that it is harmful, report it as told in [SECURITY.md](../SECURITY.md).
