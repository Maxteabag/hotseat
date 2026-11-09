"""Fixed public Go14 selection and native go-test terminal observation.
No target import, source mutation, environment or process lifecycle changes.
Raw go-test JSON remains in the controller's ordinary evidence directory.
"""
ROSTER = {'claude': ['TestSwitchPythonHeldActivationRefusesThenReadsPublishedToken', 'TestSwitchHoldsSamePythonMutexAcrossReadSnapshotActivate', 'TestActivationReleasesAfterHolderDeathAndRejectsReboundOrSymlink', 'TestSwitchUncontendedCustomRootRoutingRemainsLegacy', 'TestActivationDescriptorDoesNotSurviveUnrelatedExec', 'TestSwitchCanonicalProfileWaitSharesOriginalAdmissionBudget', 'TestSwitchRefusesWithoutAcknowledgement', 'TestSwitchIsNotSupportedOnKeychainBackends', 'TestSwitchUnknownProfile', 'TestSwitchRetargetsTheLiveFilesAndSnapshotsTheOutgoingAccount', 'TestSwitchNeverSnapshotsIntoAnUnrelatedProfile', 'TestSwitchWithoutIdentityFileLeavesClaudeJSONAlone', 'TestSwitchReportsAnUnreadableIdentityConfig', 'TestSwitchRefusesWhenProfilesCannotBeListed']}


def terminal_outcomes(events, names):
    top = [event for event in events if event.get("Test") and "/" not in event["Test"]
           and event.get("Action") in {"pass", "fail", "skip"}]
    if sorted(event["Test"] for event in top) != sorted(names) or any(event["Action"] != "pass" for event in top):
        raise ValueError("missing_duplicate_skipped_or_failed_terminal")
    if any(event.get("Action") in {"fail", "skip"} for event in events):
        raise ValueError("subtest_or_package_failure")
    return top
