# Vesper Observatory Night Operations Guide

Version: 1.0  
Applies to: staffed night observations and enclosure operations

## 1. Evidence and role separation

An observing-session record may name a shift astronomer, an instrument operator, and an equipment engineer. Naming a person for one role does not assign another role and does not show that the person's work has started or finished.

A detector replacement is complete only when both the closed maintenance work order and the post-replacement calibration trace are present. A plan, role assignment, quarantine flag, or missing alarm is not completion evidence.

## 2. Enclosure weather response

A critical enclosure condition exists when relative humidity is at least 84 percent and sustained wind is at least 14 metres per second within the same five-minute interval. When both measurements are confirmed, close the dome and notify the shift astronomer and the site safety coordinator. Humidity alone is an advisory condition and does not establish the critical condition.

Dome closure is confirmed only when the door interlock reports `CLOSED` and a matching controller event is recorded. The absence of a new alarm, a recommendation to close, or a scheduled inspection does not prove that closure occurred.

## 3. Instrument constraints

Filter L9 must not be selected when the instrument bay temperature is below minus 6 degrees Celsius. At or above that temperature, this guide adds no L9 temperature restriction; other observing constraints may still apply.

Dark-frame calibration must begin within 20 minutes after the related science series ends. A scheduled calibration is pending work, not a completed calibration trace.

## 4. Retention

Raw science frames are retained for 75 days. Quick-look preview images are retained for 12 days. Enclosure controller logs are retained for 120 days. These periods are independent and must not be substituted for one another.

## 5. Quarantine and power operations

An instrument quarantine flag prevents further science acquisition but does not prove that the instrument was powered down. After quarantine, do not power-cycle the instrument until an equipment engineer records clearance. Explaining this restriction or drafting a recovery plan does not issue a command or establish that a power cycle occurred.
