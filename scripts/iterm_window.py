WINDOW_EXISTS = r'''
on run argv
  set wantedWindow to item 1 of argv
  tell application "iTerm"
    repeat with candidateWindow in windows
      if (id of candidateWindow as text) is wantedWindow then return "present"
    end repeat
  end tell
  return "missing"
end run
'''


UI_CENSUS = r'''
tell application "System Events"
  set reportText to ""
  repeat with candidateProcess in application processes
    try
      set processName to name of candidateProcess
      repeat with candidateWindow in windows of candidateProcess
        try
          set windowText to ""
          try
            set windowText to (name of candidateWindow as text) & " " & ((value of every static text of candidateWindow) as text)
          end try
          if processName is "iTerm2" or processName is "iTerm" or windowText contains "local networks" then
            set reportText to reportText & "process " & processName & linefeed
            set reportText to reportText & "  window name=" & (name of candidateWindow as text) & linefeed
            try
              set reportText to reportText & "    AXWindowNumber=" & (value of attribute "AXWindowNumber" of candidateWindow as text) & linefeed
            on error errorText number errorNumber
              set reportText to reportText & "    AXWindowNumber error " & errorNumber & " " & errorText & linefeed
            end try
            try
              set reportText to reportText & "    AXModal=" & (value of attribute "AXModal" of candidateWindow as text) & linefeed
            on error errorText number errorNumber
              set reportText to reportText & "    AXModal error " & errorNumber & " " & errorText & linefeed
            end try
            set reportText to reportText & "    sheets=" & (count of sheets of candidateWindow) & linefeed
            repeat with candidateSheet in sheets of candidateWindow
              set reportText to reportText & "    sheet text=" & ((value of every static text of candidateSheet) as text) & linefeed
              set reportText to reportText & "    sheet buttons=" & ((name of every button of candidateSheet) as text) & linefeed
            end repeat
            set reportText to reportText & "    buttons=" & ((name of every button of candidateWindow) as text) & linefeed
            set reportText to reportText & "    text=" & ((value of every static text of candidateWindow) as text) & linefeed
          end if
        on error errorText number errorNumber
          set reportText to reportText & "  window inspection error " & errorNumber & " " & errorText & linefeed
        end try
      end repeat
    on error errorText number errorNumber
      set reportText to reportText & "process inspection error " & errorNumber & " " & errorText & linefeed
    end try
  end repeat
  return reportText
end tell
'''


ACKNOWLEDGE_WARNING = r'''
on run argv
  set wantedWindow to item 1 of argv
  set wantedSession to item 2 of argv
  set wantedName to missing value
  set matchingWindows to 0
  tell application "iTerm"
    repeat with candidateWindow in windows
      if (id of candidateWindow as text) is wantedWindow then
        set currentRef to current session of candidateWindow
        if currentRef is missing value then return "owned current session missing"
        if (unique id of currentRef) is not wantedSession then return "owned session changed"
        set wantedName to name of candidateWindow
      end if
    end repeat
    if wantedName is missing value or wantedName is "" then return "owned window name unavailable"
    repeat with candidateWindow in windows
      if (name of candidateWindow) is wantedName then set matchingWindows to matchingWindows + 1
    end repeat
  end tell
  if matchingWindows is not 1 then return "iTerm window name is ambiguous: " & wantedName
  tell application "System Events"
    if not (exists process "iTerm2") then return "iTerm process unavailable"
    tell process "iTerm2"
      set matchingUIWindows to every window whose name is wantedName
      if (count of matchingUIWindows) is not 1 then return "accessibility window name is ambiguous: " & wantedName
      set ownedUIWindow to item 1 of matchingUIWindows
      repeat with candidateDialog in sheets of ownedUIWindow
        set dialogText to ""
        try
          set dialogText to (value of every static text of candidateDialog) as text
        end try
        if dialogText is "WarningA session ended very soon after starting. Check that the command in profile \"Default\" is correct." or dialogText is "WarningA session ended very soon after starting. Check that the command in profile “Default” is correct." then
          if exists button "OK" of candidateDialog then
            click button "OK" of candidateDialog
            return "acknowledged owned short-session warning on " & wantedName & ": " & dialogText
          end if
        end if
      end repeat
    end tell
  end tell
  return "no attributable short-session warning"
end run
'''


VERIFY_RESIDUE = r'''
on run argv
  set wantedWindow to item 1 of argv
  set wantedSession to item 2 of argv
  set wantedAlternate to item 3 of argv
  set numericMatches to 0
  set alternateMatches to 0
  tell application "iTerm"
    repeat with candidateWindow in windows
      if (id of candidateWindow as text) is wantedWindow then set numericMatches to numericMatches + 1
      if (alternate identifier of candidateWindow as text) is wantedAlternate then set alternateMatches to alternateMatches + 1
      repeat with candidateTab in tabs of candidateWindow
        repeat with candidateSession in sessions of candidateTab
          if (unique id of candidateSession) is wantedSession then error "Original session is still present."
        end repeat
      end repeat
    end repeat
    if numericMatches is not 1 or alternateMatches is not 1 then error "Window identity is ambiguous."
    repeat with candidateWindow in windows
      if (id of candidateWindow as text) is wantedWindow then
        if (alternate identifier of candidateWindow as text) is not wantedAlternate then error "Alternate window identity changed."
        if (count of tabs of candidateWindow) is not 0 then error "Owned window still has tabs."
        if (current session of candidateWindow) is not missing value then error "Owned window has a current session."
        if (visible of candidateWindow) is not false then error "Owned window is visible."
        return "verified" & linefeed & (name of candidateWindow as text)
      end if
    end repeat
    error "Owned window is missing."
  end tell
end run
'''
